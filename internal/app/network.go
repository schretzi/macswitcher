package app

import (
	"bufio"
	"errors"
	"fmt"
	"net"
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
	// Non-fatal: see updateProxyBypassDomains.
	if err := updateProxyBypassDomains(services, proxyBypassDomains(cfg.LocalProxy.NoProxy)); err != nil {
		fmt.Fprintf(os.Stderr, "warning: %v\n", err)
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
	logf("local proxy set\n")
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
	// Back to what macOS ships. The list is inert while the proxy is off, but
	// leaving macswitcher's entries behind would strand configuration that
	// nothing maintains any more.
	if err := updateProxyBypassDomains(services, defaultProxyBypassDomains); err != nil {
		fmt.Fprintf(os.Stderr, "warning: %v\n", err)
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
	logf("local proxy unset\n")
	return nil
}

func proxyEnvValues(cfg Config) (string, string) {
	proxyURL := fmt.Sprintf("http://%s:%d", cfg.LocalProxy.Host, cfg.LocalProxy.Port)
	noProxy := strings.Join(cfg.LocalProxy.NoProxy, ",")
	return proxyURL, noProxy
}

// macOS ships these two in every service's bypass list: *.local for mDNS and
// 169.254/16 for link-local. They are not in no_proxy because Go's matching
// understands neither, so they are re-added unconditionally rather than lost
// the first time macswitcher writes the list.
var defaultProxyBypassDomains = []string{"*.local", "169.254/16"}

// proxyBypassDomains converts no_proxy into the syntax
// `networksetup -setproxybypassdomains` expects.
//
// This is the fourth and least obvious layer of proxy exclusion, and the only
// one that reaches GUI applications: browsers and Cocoa apps read the system
// bypass list and never see NO_PROXY. It also works in every proxy mode, which
// filter_proxy.direct does not - that PAC is only in the path in direct mode
// (see filterProxyAppliesTo), so under VPN a browser would otherwise send an
// internal host to the corporate proxy, which cannot route to it.
//
// The two syntaxes differ in a way that silently breaks things: Go matches
// `kiac` against domain labels, so it covers `kiac` and `*.kiac`, whereas
// networksetup matches literally unless there is a `*`. Each name therefore
// becomes both the bare form and a `*.` wildcard. IP literals and CIDR blocks
// are passed through untouched - a wildcard on those means nothing.
func proxyBypassDomains(noProxy []string) []string {
	out := make([]string, 0, len(noProxy)*2+len(defaultProxyBypassDomains))
	seen := make(map[string]bool)
	add := func(s string) {
		if s == "" || seen[s] {
			return
		}
		seen[s] = true
		out = append(out, s)
	}

	for _, d := range defaultProxyBypassDomains {
		add(d)
	}

	for _, entry := range noProxy {
		e := strings.TrimSpace(entry)
		if e == "" || e == "*" {
			continue
		}
		if strings.HasPrefix(e, "*") {
			add(e)
			continue
		}
		bare := strings.TrimPrefix(e, ".")
		if bare == "" {
			continue
		}
		add(bare)
		// An address is never a parent domain, so a wildcard would only add
		// an entry that can never match.
		if net.ParseIP(bare) != nil || strings.Contains(bare, "/") {
			continue
		}
		add("*." + bare)
	}
	return out
}

// updateProxyBypassDomains publishes the bypass list to every managed network
// service. Non-fatal by contract: the caller warns and carries on, because a
// browser that goes through the proxy is a smaller problem than a half
// configured network - the same reasoning as updateLaunchdProxy.
func updateProxyBypassDomains(services, domains []string) error {
	if len(domains) == 0 {
		// networksetup takes "Empty" to mean "clear the list"; passing no
		// arguments at all is a usage error.
		domains = []string{"Empty"}
	}
	var errs []error
	for _, svc := range services {
		args := append([]string{"-setproxybypassdomains", svc}, domains...)
		if err := runCommand("networksetup", args...); err != nil {
			errs = append(errs, fmt.Errorf("set proxy bypass domains for %q: %w", svc, err))
		}
	}
	return errors.Join(errs...)
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
		logf("warning: dscacheutil -flushcache failed (add \"NOPASSWD: /usr/bin/dscacheutil -flushcache\" to sudoers?): %v\n", err)
	}
	if err := runCommand("sudo", "-n", "/usr/bin/killall", "-HUP", "mDNSResponder"); err != nil {
		logf("warning: killall -HUP mDNSResponder failed (add \"NOPASSWD: /usr/bin/killall -HUP mDNSResponder\" to sudoers?): %v\n", err)
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
			logf("waiting for %s to resolve (up to %s)...\n", host, dnsResolveTimeout)
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
		logf("warning: no applications.adguardhome.restart configured; AdGuard Home may keep serving stale upstreams\n")
		return
	}
	if err := runApplicationAction(appAdGuard, actionRestart, commands); err != nil {
		logf("warning: could not restart AdGuard Home: %v\n", err)
	}
}

// writeUnboundForwarders rewrites unbound's forwarders file with a single
// "forward-zone: name: \".\"" block listing forwarders as forward-addr
// entries. Only called when dns.backend is "unbound" and
// unbound.forwarders_file is set; a machine running unbound only to keep it
// around for later, or running it side by side with AdGuard Home while
// dns.backend stays "adguard", never has this called and the file is left
// exactly as it is.
func writeUnboundForwarders(cfg Config, forwarders []string) error {
	path := strings.TrimSpace(cfg.Unbound.ForwardersFile)
	if path == "" {
		return errors.New("unbound.forwarders_file is empty")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return err
	}
	info, err := os.Lstat(path)
	if err == nil && info.Mode()&os.ModeSymlink != 0 {
		if err := os.Remove(path); err != nil {
			return fmt.Errorf("remove forwarders symlink: %w", err)
		}
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect forwarders file: %w", err)
	}
	lines := []string{
		"# Managed by macswitcher, rewritten on every switch while dns.backend is unbound.",
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
	return os.WriteFile(path, []byte(content), 0o644) // #nosec G306 -- must stay readable by the unbound service, which may run under a different system user
}

// currentUnboundForwarders reads back the forward-addr entries already in
// path, for `config init` (to seed a fresh home context) and `observe` (to
// show unbound's current forwarders). Returns nil, without error, if path is
// empty or unreadable - both are normal states for a machine that has not
// configured unbound.
func currentUnboundForwarders(path string) []string {
	path = strings.TrimSpace(path)
	if path == "" {
		return nil
	}
	data, err := os.ReadFile(path) // #nosec G304 -- path comes from the operator's own config
	if err != nil {
		return nil
	}
	var out []string
	for line := range strings.SplitSeq(string(data), "\n") {
		trimmed := strings.TrimSpace(line)
		if addr, ok := strings.CutPrefix(trimmed, "forward-addr:"); ok {
			out = append(out, strings.TrimSpace(addr))
		}
	}
	return out
}

// restartUnboundIfConfigured runs the applications.unbound restart command
// (if configured) after forwarders.conf has been rewritten, so unbound
// actually picks up the new forward-addr entries instead of continuing to
// answer from stale cached upstreams. Best-effort for the same reason as
// restartAdGuardIfConfigured: checkDNSResolution is what actually gates the
// switch.
func restartUnboundIfConfigured(cfg Config) {
	commands, ok := cfg.Applications[appUnbound]
	if !ok || len(commands.Restart) == 0 {
		logf("warning: no applications.unbound.restart configured; unbound may keep serving stale forwarders\n")
		return
	}
	if err := runApplicationAction(appUnbound, actionRestart, commands); err != nil {
		logf("warning: could not restart unbound: %v\n", err)
	}
}
