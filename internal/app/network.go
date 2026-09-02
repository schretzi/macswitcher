package app

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// How long checkDNSResolution keeps retrying, how often, and how long a single
// probe may take. Sized for a VPN tunnel coming up: launchd returns as soon as
// the agent is started, but the tunnel's routes and resolvers land seconds
// later. The per-probe timeout matters as much as the total: dscacheutil hangs
// rather than fails when the resolvers it was just pointed at are unreachable,
// so at commandTimeout a single wedged probe would eat the whole retry budget.
var (
	dnsResolveTimeout      = 60 * time.Second
	dnsResolvePollInterval = 2 * time.Second
	dnsResolveProbeTimeout = 5 * time.Second
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
	// Empty unless the filtering proxy is in the path for this context, which
	// is what turns PROXY_STATE from "on" into "filtered".
	filterAddr := ""
	if ctx, ok := cfg.Contexts[cfg.CurrentContext]; ok && filterProxyAppliesTo(cfg, ctx) {
		filterAddr = filterProxyAddr(cfg)
	}
	if err := updateZshProxy(proxyURL, noProxy, true, filterAddr); err != nil {
		return err
	}
	if err := updateDockerProxy(proxyURL, noProxy, true); err != nil {
		return err
	}
	// Non-fatal: an agent without its proxy is a smaller problem than a
	// half-configured network. See updateLaunchdProxy.
	if err := updateLaunchdProxy(proxyURL, noProxy, true); err != nil {
		fmt.Fprintf(os.Stderr, "warning: %v\n", err)
	}
	restartProxyConsumers(cfg)
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
	if err := updateZshProxy(proxyURL, noProxy, false, ""); err != nil {
		return err
	}
	if err := updateDockerProxy("", "", false); err != nil {
		return err
	}
	if err := updateLaunchdProxy("", "", false); err != nil {
		fmt.Fprintf(os.Stderr, "warning: %v\n", err)
	}
	restartProxyConsumers(cfg)
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
	// Search domains are set alongside the resolvers, and always set: see
	// searchDomainArgs for why a context that names none must still clear
	// them.
	for _, svc := range services {
		args := append([]string{"-setdnsservers", svc}, resolvers...)
		if err := runCommand("networksetup", args...); err != nil {
			return fmt.Errorf("set dns for %s: %w", svc, err)
		}
		if err := runCommand("networksetup", searchDomainArgs(cfg, svc)...); err != nil {
			return fmt.Errorf("set search domains for %s: %w", svc, err)
		}
	}
	return nil
}

// searchDomainArgs builds the networksetup call that applies the active
// context's DNS search list to one network service.
//
// The list is always set, never merely left alone: a context naming no search
// domains must actively clear whatever the previous one left behind, or a
// corporate suffix follows the machine home and silently completes
// single-label names against a network that is no longer there.
//
// These exist because a corporate PAC may nominate its proxy by short name
// ("PROXY proxy:8080"). Nothing completes such a name except the search list,
// so without it the proxy is unresolvable and every request through it fails -
// surfacing as a 502 from the local proxy, which names the wrong culprit.
func searchDomainArgs(cfg Config, service string) []string {
	args := []string{"-setsearchdomains", service}
	if ctx, ok := cfg.Contexts[cfg.CurrentContext]; ok && len(ctx.DNS.SearchDomains) > 0 {
		return append(args, ctx.DNS.SearchDomains...)
	}
	// networksetup has no "clear" verb; the literal "Empty" is how it spells
	// one.
	return append(args, "Empty")
}

// flushDNSCache flushes macOS's system DNS cache (dscacheutil) and asks
// mDNSResponder to reload (SIGHUP), the standard two-step "flush_dns"
// sequence needed after DNS servers or upstreams change - without
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
// switch proceeds any further. Which name proves that depends on the context:
//
//   - dns.check_host, when set. The only option that works on a network whose
//     resolvers serve the intranet and nothing else, where both defaults below
//     test something the network was never going to answer.
//   - the forward proxy's own hostname, in forward mode, since the rest of the
//     switch is pointless if the upstream proxy itself cannot be resolved.
//   - google.com otherwise.
//
// It shells out to dscacheutil rather than using net.LookupHost because release
// builds have CGO_ENABLED=0, so Go's resolver falls back to reading
// /etc/resolv.conf, which macOS does not keep in sync with the resolvers set
// via `networksetup -setdnsservers`.
//
// It retries until dnsResolveTimeout. A context that starts a VPN does so in
// the step before this one, and launchd reporting the agent as started says
// nothing about the tunnel's routes and resolvers being usable yet - the names
// this checks only resolve once they are. Failing on the first attempt would
// make such a context unswitchable.
func checkDNSResolution(ctx SwitchContext) error {
	host, err := dnsCheckHost(ctx)
	if err != nil {
		return err
	}
	deadline := time.Now().Add(dnsResolveTimeout)
	for attempt := 1; ; attempt++ {
		err := resolveHostOnce(host)
		if err == nil {
			return nil
		}
		if !time.Now().Before(deadline) {
			return err
		}
		if attempt == 1 {
			fmt.Printf("waiting for %s to resolve (up to %s)...\n", host, dnsResolveTimeout)
		}
		time.Sleep(dnsResolvePollInterval)
	}
}

// dnsCheckHost picks the name checkDNSResolution proves DNS with. An explicit
// dns.check_host wins over the forward proxy's hostname: a context sets it
// precisely because the defaults do not hold on that network.
func dnsCheckHost(ctx SwitchContext) (string, error) {
	if host := strings.TrimSpace(ctx.DNS.CheckHost); host != "" {
		return host, nil
	}
	if isForwardProxyMode(ctx.ProxyMode) && ctx.ForwarderProxy != nil {
		host := strings.TrimSpace(ctx.ForwarderProxy.ProxyServer)
		if host == "" {
			return "", errors.New("proxy_mode is forward but forwarder_proxy.proxy_server is empty")
		}
		return host, nil
	}
	return "google.com", nil
}

// resolveHostOnce is a single resolution attempt. dscacheutil hangs rather
// than fails when the resolvers it was just pointed at are unreachable, so the
// probe is bounded well inside commandTimeout and a kill is reported as the
// error - leaving the retry loop free to try again.
func resolveHostOnce(host string) error {
	out, err := runCommandOutputTimeout(dnsResolveProbeTimeout, "dscacheutil", "-q", "host", "-a", "name", host)
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
// upstream_dns_file - the resolvers this network wants for everything that
// is not answered locally.
//
// It rewrites only the plain upstream lines. Every domain-specific
// "[/zone/]addr" line is read back and written out again untouched, because
// those are not macswitcher's to own: an overlay contributes them for its
// intranet zones. Dropping them on a context switch would silently take the
// intranet off the air, which is exactly the kind of failure that only shows
// up an hour later.
//
// Leave adguard.upstreams_file empty and this is never called.
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
		"# Per-domain upstreams below: not macswitcher's, preserved as found.",
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
// the AdGuard restart: checkDNSResolution is what actually gates the
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
