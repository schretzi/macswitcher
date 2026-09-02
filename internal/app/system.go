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

// proxyEnvNames are the variables published to launchd, in the order they are
// written. Both cases are set because Go's http.ProxyFromEnvironment accepts
// either, but other runtimes in this setup are not so tolerant - curl reads
// only the lowercase spelling, and some Python tooling only the uppercase.
//
// HTTP_PROXY is deliberately absent in its lowercase form's usual company:
// CGI environments treat lowercase http_proxy as attacker-controlled, but a
// launchd agent is not a CGI, so the pair is safe here.
var proxyEnvNames = []struct{ proxy, noProxy string }{
	{proxy: "HTTP_PROXY", noProxy: "NO_PROXY"},
	{proxy: "HTTPS_PROXY", noProxy: ""},
	{proxy: "http_proxy", noProxy: "no_proxy"},
	{proxy: "https_proxy", noProxy: ""},
}

// Stubbed in tests.
var runLaunchctl = func(args ...string) error { return runCommand("launchctl", args...) }

// updateLaunchdProxy publishes the proxy environment into the user's launchd
// GUI domain. This is the only way an agent started by launchd can see it:
// launchd does not source the shell's rc files, so an agent inherits an
// environment holding little more than PATH - the ~/.zsh/rcs/proxy file that
// updateZshProxy writes is invisible to it.
//
// The failure this fixes is not obviously a proxy failure. A Go program whose
// transport has no proxy resolves the target host itself and reports
// "no such host", which reads like broken DNS; with a proxy it never resolves
// the name at all and hands it to the proxy instead. tunneling's IAP dialer
// failing every tunnel with `lookup oauth2.googleapis.com: no such host`,
// while the same binary worked from a shell, is what this is for.
//
// Two limits worth knowing. launchctl setenv only reaches processes started
// *after* it runs, so a context switch has to restart the agents that care -
// that is what apps.restart is for. And Go caches the environment on the
// first ProxyFromEnvironment call, so even a live agent would not pick up a
// change without restarting. Both point the same way: list the agent in
// apps.restart rather than expecting it to notice.
//
// Non-fatal by design. Every other step here writes a file in the user's own
// home and effectively cannot fail, but launchctl needs a GUI domain to talk
// to; over SSH there is none. Aborting the switch there would leave the
// network half-configured, which is worse than an agent missing its proxy.
func updateLaunchdProxy(proxyURL, noProxy string, enable bool) error {
	if noProxy == "" {
		noProxy = "localhost," + loopbackLocal + "," + loopbackIPv6
	}
	for _, name := range proxyEnvNames {
		vars := []struct{ key, value string }{{key: name.proxy, value: proxyURL}}
		if name.noProxy != "" {
			vars = append(vars, struct{ key, value string }{key: name.noProxy, value: noProxy})
		}
		for _, v := range vars {
			var err error
			if enable {
				err = runLaunchctl("setenv", v.key, v.value)
			} else {
				err = runLaunchctl("unsetenv", v.key)
			}
			if err != nil {
				return fmt.Errorf("publish %s to launchd: %w", v.key, err)
			}
		}
	}
	return nil
}

// restartProxyConsumers restarts the agents named in local_proxy.restart_agents
// so they pick up the environment updateLaunchdProxy just published.
//
// Best effort per agent, and it keeps going after a failure: these are
// conveniences layered on top of a network change that has already succeeded,
// and one agent refusing to restart is no reason to report the switch itself
// as failed.
func restartProxyConsumers(cfg Config) {
	for _, name := range cfg.LocalProxy.RestartAgents {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		commands, ok := cfg.Applications[name]
		if !ok {
			fmt.Fprintf(os.Stderr, "warning: local_proxy.restart_agents names %q, which is not defined under applications\n", name)
			continue
		}
		if err := runApplicationAction(name, actionRestart, commands); err != nil {
			fmt.Fprintf(os.Stderr, "warning: could not restart %q for the new proxy environment: %v\n", name, err)
		}
	}
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
