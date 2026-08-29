package app

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

func setLocalProxy(cfgPath string) error {
	cfg, err := loadConfig(cfgPath)
	if err != nil {
		return err
	}
	proxyURL, noProxy := proxyEnvValues(cfg)
	services, err := resolveNetworkServices(cfg)
	if err != nil {
		return err
	}
	for _, svc := range services {
		if err := runCommand("networksetup", "-setwebproxy", svc, cfg.LocalProxy.Host, strconv.Itoa(cfg.LocalProxy.Port)); err != nil {
			return err
		}
		if err := runCommand("networksetup", "-setsecurewebproxy", svc, cfg.LocalProxy.Host, strconv.Itoa(cfg.LocalProxy.Port)); err != nil {
			return err
		}
		if err := runCommand("networksetup", "-setwebproxystate", svc, "on"); err != nil {
			return err
		}
		if err := runCommand("networksetup", "-setsecurewebproxystate", svc, "on"); err != nil {
			return err
		}
	}
	if err := updateZshProxy(proxyURL, noProxy, true); err != nil {
		return err
	}
	if err := updateDockerProxy(proxyURL, noProxy, true); err != nil {
		return err
	}
	fmt.Println("local proxy set")
	return nil
}

func unsetLocalProxy(cfgPath string) error {
	cfg, err := loadConfig(cfgPath)
	if err != nil {
		return err
	}
	proxyURL, noProxy := proxyEnvValues(cfg)
	services, err := resolveNetworkServices(cfg)
	if err != nil {
		return err
	}
	for _, svc := range services {
		if err := runCommand("networksetup", "-setwebproxystate", svc, "off"); err != nil {
			return err
		}
		if err := runCommand("networksetup", "-setsecurewebproxystate", svc, "off"); err != nil {
			return err
		}
	}
	if err := updateZshProxy(proxyURL, noProxy, false); err != nil {
		return err
	}
	if err := updateDockerProxy("", "", false); err != nil {
		return err
	}
	fmt.Println("local proxy unset")
	return nil
}

func proxyEnvValues(cfg Config) (string, string) {
	proxyURL := fmt.Sprintf("http://%s:%d", cfg.LocalProxy.Host, cfg.LocalProxy.Port)
	noProxy := strings.Join(cfg.LocalProxy.NoProxy, ",")
	return proxyURL, noProxy
}

func resolveNetworkServices(cfg Config) ([]string, error) {
	if len(cfg.NetworkServices) > 0 {
		return cfg.NetworkServices, nil
	}

	if ctx, ok := cfg.Contexts[cfg.CurrentContext]; ok && len(ctx.DNS.NetworkServices) > 0 {
		return ctx.DNS.NetworkServices, nil
	}
	out, err := runCommandOutput("networksetup", "-listallnetworkservices")
	if err != nil {
		return nil, err
	}
	var services []string
	sc := bufio.NewScanner(strings.NewReader(out))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "An asterisk") || strings.HasPrefix(line, "*") {
			continue
		}
		services = append(services, line)
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if len(services) == 0 {
		return nil, errors.New("no active network services found")
	}
	return services, nil
}

func listNetworkServices() ([]string, error) {
	out, err := runCommandOutput("networksetup", "-listallnetworkservices")
	if err != nil {
		return nil, err
	}
	var services []string
	sc := bufio.NewScanner(strings.NewReader(out))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "An asterisk") || strings.HasPrefix(line, "*") {
			continue
		}
		services = append(services, line)
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return services, nil
}

func applyLocalResolverDNS(cfg Config) error {
	services, err := resolveNetworkServices(cfg)
	if err != nil {
		return err
	}
	resolvers := []string{strings.TrimSpace(cfg.DNS.LocalResolver)}
	if ctx, ok := cfg.Contexts[cfg.CurrentContext]; ok && len(ctx.DNS.Resolvers) > 0 {
		resolvers = ctx.DNS.Resolvers
	}
	if resolvers[0] == "" {
		resolvers[0] = loopbackResolver
	}
	for _, svc := range services {
		args := append([]string{"-setdnsservers", svc}, resolvers...)
		if err := runCommand("networksetup", args...); err != nil {
			return fmt.Errorf("set dns for %s: %w", svc, err)
		}
	}
	return nil
}

// restartUnboundIfConfigured runs the applications.unbound restart command
// (if configured) after forwarders.conf has been rewritten, so unbound
// actually picks up the new forward-addr entries instead of continuing to
// answer from stale cached upstreams. It is best-effort: a missing restart
// command, or the command failing, only produces a warning here, because
// checkDNSResolution (run right after) is what actually verifies whether
// DNS is working before the switch is allowed to continue.
func restartUnboundIfConfigured(cfg Config) {
	commands, ok := cfg.Applications[appUnbound]
	if !ok || len(commands.Restart) == 0 {
		fmt.Println("warning: no applications.unbound.restart configured; unbound may keep serving stale forwarders")
		return
	}
	if err := runApplicationAction(appUnbound, actionRestart, commands); err != nil {
		fmt.Printf("warning: could not restart unbound: %v\n", err)
	}
}

// flushDNSCache flushes macOS's system DNS cache (dscacheutil) and asks
// mDNSResponder to reload (SIGHUP), the standard two-step "flush_dns"
// sequence needed after DNS servers or unbound forwarders change - without
// it, in-flight lookups and cached negative/stale answers can linger for
// minutes. Both commands typically need root; if the operator hasn't set up
// passwordless sudo for them, this only warns; checkDNSResolution is what
// actually decides whether the switch can continue.
func flushDNSCache() {
	if err := runCommand("sudo", "-n", "/usr/bin/dscacheutil", "-flushcache"); err != nil {
		fmt.Printf("warning: dscacheutil -flushcache failed (add \"NOPASSWD: /usr/bin/dscacheutil -flushcache\" to sudoers?): %v\n", err)
	}
	if err := runCommand("sudo", "-n", "/usr/bin/killall", "-HUP", "mDNSResponder"); err != nil {
		fmt.Printf("warning: killall -HUP mDNSResponder failed (add \"NOPASSWD: /usr/bin/killall -HUP mDNSResponder\" to sudoers?): %v\n", err)
	}
}

// checkDNSResolution verifies that DNS is actually working before the
// switch proceeds any further: google.com for off/direct proxy modes, or the
// forward proxy's own hostname for forward mode (since the rest of the
// switch is pointless if the upstream proxy itself can't be resolved). It
// shells out to dscacheutil rather than using net.LookupHost because release
// builds have CGO_ENABLED=0, so Go's resolver falls back to reading
// /etc/resolv.conf, which macOS does not keep in sync with the resolvers set
// via `networksetup -setdnsservers`.
func checkDNSResolution(ctx SwitchContext) error {
	host := "google.com"
	if isForwardProxyMode(ctx.ProxyMode) && ctx.ForwarderProxy != nil {
		host = strings.TrimSpace(ctx.ForwarderProxy.ProxyServer)
		if host == "" {
			return errors.New("proxy_mode is forward but forwarder_proxy.proxy_server is empty")
		}
	}
	out, err := runCommandOutput("dscacheutil", "-q", "host", "-a", "name", host)
	if err != nil {
		return fmt.Errorf("dscacheutil -q host -a name %s failed: %w", host, err)
	}
	if !dscacheutilOutputHasAddress(out) {
		return fmt.Errorf("%s did not resolve", host)
	}
	return nil
}

// dscacheutilOutputHasAddress reports whether dscacheutil's `-q host`
// output contains at least one resolved address. dscacheutil always exits 0
// (even for an unresolvable host), so success has to be judged from the
// output content, not the exit code.
func dscacheutilOutputHasAddress(out string) bool {
	return strings.Contains(out, "ip_address:") || strings.Contains(out, "ipv6_address:")
}

// writeAdGuardUpstreams rewrites the default upstreams in AdGuard Home's
// upstream_dns_file, which is its equivalent of unbound's forwarders.conf.
//
// It rewrites only the plain upstream lines. Every domain-specific
// "[/zone/]addr" line is read back and written out again untouched, because
// those are not macswitcher's to own: they come from the unbound forward-zones
// that MacbookSetup/Scripts/MacOS/unbound_to_adguard.py translates (the work
// overlay's intranet zones in particular). Dropping them on a context switch
// would silently take the intranet off the air, which is exactly the kind of
// failure that only shows up an hour later.
//
// A machine that has not opted into the AdGuard Home trial leaves
// adguard.upstreams_file empty, and this is never called.
func writeAdGuardUpstreams(cfg Config, forwarders []string, contextName string) error {
	path := strings.TrimSpace(cfg.AdGuard.UpstreamsFile)
	if path == "" {
		return errors.New("adguard.upstreams_file is empty")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return err
	}

	specific := preservedAdGuardUpstreams(path)

	info, err := os.Lstat(path)
	if err == nil && info.Mode()&os.ModeSymlink != 0 {
		if err := os.Remove(path); err != nil {
			return fmt.Errorf("remove upstreams symlink: %w", err)
		}
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect upstreams file: %w", err)
	}

	// AdGuard Home only treats a line as a comment when it starts with '#'; a
	// trailing "1.1.1.1 # note" parses the '#' as a second upstream address
	// and is rejected. So every comment gets its own line.
	lines := []string{
		"# Default upstreams: managed by macswitcher, rewritten on every switch.",
		"# Per-domain upstreams below: generated by unbound_to_adguard.py, preserved here.",
		"#",
		"# context: " + contextName,
		"",
	}
	for _, fwd := range forwarders {
		f := strings.TrimSpace(fwd)
		if f == "" {
			continue
		}
		lines = append(lines, f)
	}
	if len(specific) > 0 {
		lines = append(lines, "", "# --- per-domain upstreams (not managed by macswitcher) ---")
		lines = append(lines, specific...)
	}
	content := strings.Join(lines, "\n") + "\n"
	return os.WriteFile(path, []byte(content), 0o644) // #nosec G306 -- must stay readable by the AdGuard Home service, which runs as root but may be inspected by the operator
}

// preservedAdGuardUpstreams returns the domain-specific "[/zone/]addr" lines
// already in the upstream file, which a rewrite must carry over.
func preservedAdGuardUpstreams(path string) []string {
	data, err := os.ReadFile(path) // #nosec G304 -- path comes from the operator's own config
	if err != nil {
		return nil
	}
	var out []string
	for line := range strings.SplitSeq(string(data), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "[") {
			out = append(out, strings.TrimSpace(line))
		}
	}
	return out
}

// restartAdGuardIfConfigured restarts AdGuard Home after its upstream file has
// been rewritten. AdGuard Home reads upstream_dns_file at startup only - there
// is no reload signal for it - so without this it keeps forwarding to the
// previous network's resolvers. Best-effort for the same reason as
// restartUnboundIfConfigured: checkDNSResolution is what actually gates the
// switch.
func restartAdGuardIfConfigured(cfg Config) {
	commands, ok := cfg.Applications[appAdGuard]
	if !ok || len(commands.Restart) == 0 {
		fmt.Println("warning: no applications.adguardhome.restart configured; AdGuard Home may keep serving stale upstreams")
		return
	}
	if err := runApplicationAction(appAdGuard, actionRestart, commands); err != nil {
		fmt.Printf("warning: could not restart AdGuard Home: %v\n", err)
	}
}

func writeUnboundForwarders(cfg Config, forwarders []string) error {
	if strings.TrimSpace(cfg.Unbound.ForwardersFile) == "" {
		return errors.New("unbound.forwarders_file is empty")
	}
	if err := os.MkdirAll(filepath.Dir(cfg.Unbound.ForwardersFile), 0o750); err != nil {
		return err
	}
	info, err := os.Lstat(cfg.Unbound.ForwardersFile)
	if err == nil && info.Mode()&os.ModeSymlink != 0 {
		if err := os.Remove(cfg.Unbound.ForwardersFile); err != nil {
			return fmt.Errorf("remove forwarders symlink: %w", err)
		}
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect forwarders file: %w", err)
	}
	lines := []string{
		"# Managed by macswitcher",
		"forward-zone:",
		"  name: \".\"",
	}
	for _, fwd := range forwarders {
		f := strings.TrimSpace(fwd)
		if f == "" {
			continue
		}
		lines = append(lines, "  forward-addr: "+f)
	}
	content := strings.Join(lines, "\n") + "\n"
	return os.WriteFile(cfg.Unbound.ForwardersFile, []byte(content), 0o644) // #nosec G306 -- must stay readable by the unbound service, which may run under a different system user
}
