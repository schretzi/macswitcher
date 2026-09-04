package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os/exec"
	"strings"
	"time"
)

// AdGuard Home's filtering switch, driven per context.
//
// THE PROBLEM THIS SOLVES
// On a corporate network the machine is in a bootstrap deadlock: AdGuard Home
// wants to fetch its filter lists, that needs the proxy, the proxy needs DNS,
// and DNS *is* AdGuard Home. Switching to the office context therefore came up
// with no working DNS at all, and the only way out was turning filtering off
// by hand in the web UI. Filtering there is redundant anyway - everything is
// forwarded to corporate systems that do their own filtering.
//
// WHY THE API AND NOT THE CONFIG FILE
// protection_enabled lives in AdGuardHome.yaml, but that file is the
// runtime's: AdGuard Home rewrites it wholesale on every web-UI change, and it
// sits in a 0700 root-owned prefix, so editing it would need sudo on every
// switch and would race the daemon. POST /control/protection applies to the
// running process at once, needs no restart (so the DNS cache survives), and
// AdGuard Home persists it itself.

const (
	// adguardAPITimeout bounds a single call. The API is on loopback, so this
	// is generous; it exists so a wedged daemon cannot stall a switch.
	adguardAPITimeout = 10 * time.Second
)

// How long setAdGuardProtection keeps trying, and how often. Sized for the
// restart that syncAdGuardUpstreams performs immediately before: launchctl
// kickstart returns as soon as the process is spawned, while the HTTP listener
// comes up a moment later, so the first call can legitimately be refused.
//
// Variables rather than constants so tests do not have to spend the retry
// budget in real time, matching dnsResolveTimeout in network.go.
var (
	adguardAPIRetryFor      = 20 * time.Second
	adguardAPIRetryInterval = 1 * time.Second
)

// adguardAPIClient talks to loopback and must never go through a proxy.
//
// Proxy: nil rather than the default http.ProxyFromEnvironment. The latter
// does skip loopback addresses, but macswitcher is the process that *sets*
// HTTP_PROXY for everything else, and pointing a call at the very proxy this
// switch is in the middle of reconfiguring is a failure mode worth removing by
// construction rather than relying on someone else's special case.
func adguardAPIClient() *http.Client {
	return &http.Client{
		Timeout:   adguardAPITimeout,
		Transport: &http.Transport{Proxy: nil},
	}
}

// adguardAPIPassword reads the API credential out of the System keychain.
//
// A variable so tests can supply one without a real keychain, matching
// keychainPasswordGet in proxy.go. It is not that function: this credential
// lives in /Library/Keychains/System.keychain, named explicitly rather than
// left to the search list, whereas keychainPasswordGet reads the user's login
// keychain.
//
// Deliberately not runCommandOutput either: that returns CombinedOutput, which
// would splice any warning on stderr into the password, and trims all
// surrounding whitespace, which would corrupt a password that legitimately
// ends in one. `security -w` writes the secret to stdout with a single
// trailing newline.
var adguardAPIPassword = func(cfg Config) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), adguardAPITimeout)
	defer cancel()

	service := cfg.AdGuard.adguardAPIKeychainService()
	user := cfg.AdGuard.adguardAPIUser()
	cmd := exec.CommandContext(ctx, "security", "find-generic-password",
		"-s", service, "-a", user, "-w", "/Library/Keychains/System.keychain")
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf(
			"no AdGuard Home API credential for account %q (keychain service %q) in the System keychain: %w",
			user, service, err,
		)
	}
	password := strings.TrimRight(string(out), "\r\n")
	if password == "" {
		return "", fmt.Errorf("empty AdGuard Home API credential for account %q (keychain service %q)", user, service)
	}
	return password, nil
}

// adguardAPIURL builds an endpoint URL from the configured address.
func adguardAPIURL(cfg Config, path string) string {
	addr := cfg.AdGuard.adguardAddress()
	addr = strings.TrimSuffix(strings.TrimPrefix(strings.TrimPrefix(addr, "http://"), "https://"), "/")
	return "http://" + addr + path
}

// adguardProtectionEnabled reports whether AdGuard Home is filtering right
// now, straight from the daemon. Used by `status` so a context that turned
// filtering off cannot do it silently.
func adguardProtectionEnabled(cfg Config) (bool, error) {
	pass, err := adguardAPIPassword(cfg)
	if err != nil {
		return false, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), adguardAPITimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, adguardAPIURL(cfg, "/control/status"), nil)
	if err != nil {
		return false, err
	}
	req.SetBasicAuth(cfg.AdGuard.adguardAPIUser(), pass)

	resp, err := adguardAPIClient().Do(req)
	if err != nil {
		return false, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return false, fmt.Errorf("GET /control/status returned %s", resp.Status)
	}
	var payload struct {
		ProtectionEnabled bool `json:"protection_enabled"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return false, fmt.Errorf("decode /control/status: %w", err)
	}
	return payload.ProtectionEnabled, nil
}

// setAdGuardProtection turns filtering on or off, retrying while AdGuard Home
// is still coming back from the restart syncAdGuardUpstreams just did.
//
// A missing credential is returned immediately rather than retried: it will
// not become present within the retry window, and twenty seconds of silence is
// a poor way to report a configuration problem.
func setAdGuardProtection(cfg Config, enabled bool) error {
	pass, err := adguardAPIPassword(cfg)
	if err != nil {
		return err
	}

	body, err := json.Marshal(struct {
		Enabled bool `json:"enabled"`
		// AdGuard Home reads duration only when disabling; 0 means "until
		// something turns it back on", which is what a context switch wants -
		// a timed unpause would re-enable filtering mid-context with nothing
		// saying so.
		Duration int `json:"duration"`
	}{Enabled: enabled, Duration: 0})
	if err != nil {
		return err
	}

	client := adguardAPIClient()
	url := adguardAPIURL(cfg, "/control/protection")
	deadline := time.Now().Add(adguardAPIRetryFor)
	var lastErr error
	for {
		lastErr = adguardProtectionCall(client, url, cfg.AdGuard.adguardAPIUser(), pass, body)
		if lastErr == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return lastErr
		}
		time.Sleep(adguardAPIRetryInterval)
	}
}

func adguardProtectionCall(client *http.Client, url, user, pass string, body []byte) error {
	ctx, cancel := context.WithTimeout(context.Background(), adguardAPITimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.SetBasicAuth(user, pass)

	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("POST /control/protection returned %s", resp.Status)
	}
	return nil
}

// syncAdGuardProtection applies a context's protection_enabled, if it has one.
//
// Never fatal to a switch, and loud either way. The two failure directions are
// not symmetric, and neither is improved by aborting here:
//
//   - failing to DISABLE filtering breaks name resolution, which the DNS check
//     a few lines later catches with a hint that names this setting;
//   - failing to ENABLE it costs filtering, not connectivity, and taking the
//     network down over that would be the wrong trade - `macswitcher status`
//     reports the daemon's live state, so it does not pass unnoticed.
//
// Aborting would also make an unrelated hiccup - or a machine that simply has
// no API credential - break every switch on a config that is otherwise fine.
func syncAdGuardProtection(cfg Config, ctx SwitchContext) {
	if ctx.ProtectionEnabled == nil {
		return
	}
	want := *ctx.ProtectionEnabled
	if err := setAdGuardProtection(cfg, want); err != nil {
		logf("warning: could not turn AdGuard Home filtering %s: %v\n", enabledWord(want), err)
		if !want {
			logf("warning: filtering is still on; if DNS fails below, that is why\n")
		}
		return
	}
	logf("adguard filtering: %s\n", enabledWord(want))
}

func enabledWord(enabled bool) string {
	if enabled {
		return "on"
	}
	return "off"
}

// protectionHint is appended to the DNS-check failure. AdGuard Home failing to
// reach its filter lists is a genuinely non-obvious cause of "DNS is broken",
// and it cost an office visit to find by hand - so the error says so, but only
// when filtering is actually a candidate: a context that already asks for it
// to be off has ruled itself out.
func protectionHint(ctx SwitchContext) string {
	if ctx.ProtectionEnabled != nil && !*ctx.ProtectionEnabled {
		return ""
	}
	return "\nhint: if this network reaches the internet only through a proxy, AdGuard Home's " +
		"filter lists are unreachable until that proxy works - and the proxy needs the DNS " +
		"this is waiting on. Set `protection_enabled: false` on this context to break that loop; " +
		"filtering is redundant where everything is forwarded to a corporate proxy anyway."
}

// adguardProtectionStatusLine describes filtering for `status`: what the
// context asks for, and what the daemon actually reports. They are shown
// together on purpose - a context that asks for filtering and a daemon that is
// not filtering is exactly the state worth noticing.
func adguardProtectionStatusLine(cfg Config, ctx SwitchContext) string {
	want := "not managed by this context"
	if ctx.ProtectionEnabled != nil {
		want = enabledWord(*ctx.ProtectionEnabled)
	}
	live, err := adguardProtectionEnabled(cfg)
	if err != nil {
		var netErr net.Error
		if errors.As(err, &netErr) || strings.Contains(err.Error(), "connection refused") {
			return want + " (AdGuard Home not reachable)"
		}
		return fmt.Sprintf("%s (live state unknown: %v)", want, err)
	}
	if ctx.ProtectionEnabled != nil && *ctx.ProtectionEnabled != live {
		return fmt.Sprintf("%s requested, but AdGuard Home reports %s", want, enabledWord(live))
	}
	return fmt.Sprintf("%s (AdGuard Home reports %s)", want, enabledWord(live))
}
