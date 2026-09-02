package app

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// updateZshProxy rewrites ~/.zsh/rcs/proxy, the file the shell sources for
// its proxy environment and Starship reads for the prompt.
//
// filterAddr is the filtering proxy's host:port when it is in the path for
// this context, empty otherwise. It makes PROXY_STATE three-valued -
// off / on / filtered - because "on" alone cannot distinguish a proxy that
// filters from one that does not, and the whole point of the filter is that
// you can tell.
//
// What this deliberately does NOT claim: that the filter is answering right
// now. The file is written when the context is applied, so it describes
// routing, not liveness - a filter that dies an hour later still reads
// "filtered" here. `macswitcher status` probes the port and is the place
// that answers the other question.
func updateZshProxy(proxyURL, noProxy string, enable bool, filterAddr string) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	rcDir := filepath.Join(home, ".zsh", "rcs")
	if err := os.MkdirAll(rcDir, 0o750); err != nil {
		return err
	}
	rcPath := filepath.Join(rcDir, "proxy")

	host := ""
	port := ""
	if strings.TrimSpace(proxyURL) != "" {
		u, err := url.Parse(proxyURL)
		if err != nil {
			return fmt.Errorf("invalid proxy url %q: %w", proxyURL, err)
		}
		host = u.Hostname()
		port = u.Port()
	}
	if noProxy == "" {
		noProxy = "localhost," + loopbackLocal + "," + loopbackIPv6
	}

	state := ProxyModeOff
	switch {
	case enable && strings.TrimSpace(filterAddr) != "":
		state = proxyStateFiltered
	case enable:
		state = proxyStateOn
	}
	if !enable {
		filterAddr = ""
	}

	content := strings.Join([]string{
		"# Proxy configuration values (managed by macswitcher)",
		fmt.Sprintf("export PROXY_STATE=%q", state),
		fmt.Sprintf("export PROXY_FILTER=%q", filterAddr),
		fmt.Sprintf("export PROXY_HOST=%q", host),
		fmt.Sprintf("export PROXY_PORT=%q", port),
		fmt.Sprintf("export PROXY_URL=%q", proxyURL),
		fmt.Sprintf("export PROXY_NO_PROXY=%q", noProxy),
		fmt.Sprintf("export http_proxy=%q", proxyURL),
		fmt.Sprintf("export https_proxy=%q", proxyURL),
		fmt.Sprintf("export HTTP_PROXY=%q", proxyURL),
		fmt.Sprintf("export HTTPS_PROXY=%q", proxyURL),
		fmt.Sprintf("export no_proxy=%q", noProxy),
		fmt.Sprintf("export NO_PROXY=%q", noProxy),
		"",
	}, "\n")

	return os.WriteFile(rcPath, []byte(content), 0o600)
}

func updateDockerProxy(proxyURL, noProxy string, enable bool) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	dockerConfig := filepath.Join(home, ".docker", "config.json")
	if err := os.MkdirAll(filepath.Dir(dockerConfig), 0o750); err != nil {
		return err
	}
	obj := map[string]any{}
	if b, err := os.ReadFile(dockerConfig); err == nil { // #nosec G304 -- fixed path under the user's own home dir
		if len(strings.TrimSpace(string(b))) > 0 {
			if err := json.Unmarshal(b, &obj); err != nil {
				return fmt.Errorf("parse docker config: %w", err)
			}
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}

	proxiesAny, ok := obj["proxies"]
	var proxies map[string]any
	if ok {
		cast, ok := proxiesAny.(map[string]any)
		if !ok {
			return errors.New("docker config proxies field is not an object")
		}
		proxies = cast
	} else {
		proxies = map[string]any{}
		obj["proxies"] = proxies
	}

	if enable {
		proxies["default"] = map[string]any{
			"httpProxy":  proxyURL,
			"httpsProxy": proxyURL,
			"noProxy":    noProxy,
		}
	} else {
		delete(proxies, "default")
		if len(proxies) == 0 {
			delete(obj, "proxies")
		}
	}

	b, err := json.MarshalIndent(obj, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(dockerConfig, append(b, '\n'), 0o600)
}

func syncContextApplications(cfg Config, ctx SwitchContext) error {
	actions := []struct {
		name         string
		applications []string
	}{
		{name: actionStop, applications: ctx.Apps.Stop},
		{name: actionRestart, applications: ctx.Apps.Restart},
		{name: actionReload, applications: ctx.Apps.Reload},
		{name: actionStart, applications: ctx.Apps.Start},
	}
	for _, action := range actions {
		for _, name := range action.applications {
			name = strings.TrimSpace(name)
			if name == "" {
				continue
			}
			if err := runApplicationAction(name, action.name, cfg.Applications[name]); err != nil {
				return err
			}
			if name == appVPN && action.name != actionStop {
				waitForVPN(cfg)
			}
		}
	}
	return nil
}

// How long waitForVPN gives the tunnel, and how often it looks.
var (
	vpnUpTimeout      = 60 * time.Second
	vpnUpPollInterval = time.Second
)

// waitForVPN blocks until the VPN's interface has an address, which is the
// only signal that the tunnel actually negotiated - starting the agent merely
// means launchd accepted the job.
//
// The wait is what keeps the rest of the switch off a half-open tunnel. The
// step right after this points AdGuard Home at the context's upstreams, and
// for a VPN context those resolvers live inside the tunnel; racing openconnect
// to them takes DNS down while openconnect is still resolving its own gateway
// through it.
//
// Not fatal on timeout. A tunnel that is slow but coming is common enough that
// aborting here would be worse than continuing - and checkDNSResolution
// further down is the check that actually decides whether the switch worked.
func waitForVPN(cfg Config) {
	iface := strings.TrimSpace(cfg.Daemons.VPN.Interface)
	if iface == "" {
		return
	}
	if up, _ := vpnInterfaceStatus(iface); up {
		return
	}
	fmt.Printf("waiting for the VPN tunnel on %s (up to %s)...\n", iface, vpnUpTimeout)
	deadline := time.Now().Add(vpnUpTimeout)
	for {
		if up, detail := vpnInterfaceStatus(iface); up {
			fmt.Println(detail)
			return
		}
		if !time.Now().Before(deadline) {
			fmt.Printf("warning: the VPN tunnel on %s did not come up within %s; continuing anyway\n", iface, vpnUpTimeout)
			return
		}
		time.Sleep(vpnUpPollInterval)
	}
}

func runApplicationAction(name, action string, commands ApplicationCommands) error {
	command := applicationCommand(commands, action)
	if len(command) > 0 {
		if err := runCommandList(command); err != nil {
			return fmt.Errorf("application %q %s command failed: %w", name, action, err)
		}
		return nil
	}
	if action != actionRestart && action != actionReload {
		return fmt.Errorf("application %q has no %s command", name, action)
	}
	if len(commands.Stop) == 0 || len(commands.Start) == 0 {
		return fmt.Errorf("application %q has no %s command or stop/start fallback", name, action)
	}
	if err := runCommandList(commands.Stop); err != nil {
		return fmt.Errorf("application %q stop fallback for %s failed: %w", name, action, err)
	}
	if err := runCommandList(commands.Start); err != nil {
		return fmt.Errorf("application %q start fallback for %s failed: %w", name, action, err)
	}
	return nil
}

func applicationCommand(commands ApplicationCommands, action string) []string {
	switch action {
	case actionStart:
		return commands.Start
	case actionStop:
		return commands.Stop
	case actionRestart:
		return commands.Restart
	case actionReload:
		return commands.Reload
	default:
		return nil
	}
}
