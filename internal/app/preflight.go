package app

import (
	"errors"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
)

// Preflight: prove the names a switch depends on can be resolved BEFORE the
// switch starts, and if they cannot, try once with DHCP's own resolvers.
//
// The failure it exists for is a dead end, not a slow path. applyContext
// starts the VPN before it repoints the resolvers, and that ordering is
// correct - the tunnel has to resolve its own gateway through the network the
// machine is still on. But it makes the whole switch conditional on the
// CURRENT resolvers working. When they do not - a stale corporate resolver
// still configured after leaving the office, a local resolver that is down -
// openconnect fails with "getaddrinfo failed", applyContext returns, and
// rollbackContext dutifully restores the previous context: the one whose
// resolvers are equally broken. Every subsequent attempt fails identically.
// The machine cannot switch its way out, and the only escape is knowing to
// reach for networksetup by hand.
//
// The fix is not more retries. It is noticing, before anything is changed,
// that name resolution is broken, and handing the resolvers back to DHCP
// long enough to find out whether the network itself is fine. That is a
// question the operator would otherwise have to think of; asking it costs one
// dscacheutil probe on the good path.

// preflightResult reports what the check found, so callers can print it
// without re-deriving anything.
type preflightResult struct {
	// Hosts is what was probed, in the order it was probed.
	Hosts []string
	// OK is true when every host resolved - with the resolvers already
	// configured if UsedDHCP is false, or with DHCP's if it is true.
	OK bool
	// UsedDHCP records that the resolvers were handed back to DHCP for a
	// second attempt. Always paired with a restore, whatever the outcome.
	UsedDHCP bool
	// Failures are the errors from the LAST attempt, empty when OK.
	Failures []error
}

// dnsServiceOps is the only path from preflight to the machine's DNS
// configuration, and it is a var so tests can replace it wholesale.
//
// This is not a style preference. main_test.go arms it with stubs that panic,
// for the same reason launchdActions is armed: a test that reached the real
// implementation would set every network service on the developer's machine
// to DHCP resolvers and, if it failed to restore, leave it there. We have
// already had one `go test` run stop the operator's live proxy; that lesson
// is cheaper to apply than to relearn.
var dnsServiceOps = dnsServiceOpsExec()

type dnsOps struct {
	// get returns the resolvers configured for one network service, or an
	// empty slice when the service is on DHCP.
	get func(service string) ([]string, error)
	// set applies resolvers to one network service. An empty slice means
	// "hand it back to DHCP".
	set func(service string, resolvers []string) error
	// flush drops the system DNS cache. Part of this struct rather than a
	// direct call so that stubbing dnsServiceOps covers EVERYTHING preflight
	// does to the machine - a stub with a hole in it is a stub that gets
	// found out on someone's laptop.
	flush func()
	// resolve is one resolution attempt for one name. Stubbed alongside the
	// rest so a test can describe a network instead of depending on one.
	resolve func(host string) error
}

func dnsServiceOpsExec() dnsOps {
	return dnsOps{
		get:     getDNSServers,
		set:     setDNSServers,
		flush:   flushDNSCache,
		resolve: resolveHostOnce,
	}
}

// noDNSServersPrefix is how networksetup spells "this service is on DHCP".
// It answers -getdnsservers with a SENTENCE rather than an empty list or a
// non-zero exit:
//
//	There aren't any DNS Servers set on Wi-Fi.
//
// Parsed naively that is a resolver called "There", which would then be
// written back on restore - turning a service that was correctly on DHCP into
// one pointing at a nonexistent nameserver, permanently. The restore path is
// the one that must never be wrong, so the sentence is recognised here.
const noDNSServersPrefix = "There aren't any"

func getDNSServers(service string) ([]string, error) {
	out, err := runCommandOutput("networksetup", "-getdnsservers", service)
	if err != nil {
		return nil, fmt.Errorf("get dns for %s: %w", service, err)
	}
	var resolvers []string
	for line := range strings.SplitSeq(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, noDNSServersPrefix) {
			continue
		}
		resolvers = append(resolvers, line)
	}
	return resolvers, nil
}

func setDNSServers(service string, resolvers []string) error {
	args := append([]string{"-setdnsservers", service}, dnsServerArgs(resolvers)...)
	if err := runCommand("networksetup", args...); err != nil {
		return fmt.Errorf("set dns for %s: %w", service, err)
	}
	return nil
}

// dnsServerArgs turns a resolver list into networksetup's arguments.
// networksetup has no "clear" verb; the literal "Empty" is how it spells one,
// the same way searchDomainArgs uses it.
func dnsServerArgs(resolvers []string) []string {
	if len(resolvers) == 0 {
		return []string{"Empty"}
	}
	return resolvers
}

// preflightHosts is the list of names a switch into ctx must be able to
// resolve, deduplicated and in a stable order.
//
// Derived from what the context already declares, plus whatever preflight_hosts
// adds. The derived part is deliberately limited to names macswitcher itself
// knows the meaning of. In particular the VPN gateway is NOT derived: it lives
// in an employer-specific file read by a shell script, and teaching macswitcher
// to look there would make a general tool carry one employer's layout. It
// belongs in preflight_hosts, where it is data.
func preflightHosts(ctx SwitchContext) []string {
	var hosts []string
	add := func(host string) {
		host = strings.TrimSpace(host)
		if host == "" {
			return
		}
		for _, existing := range hosts {
			if strings.EqualFold(existing, host) {
				return
			}
		}
		hosts = append(hosts, host)
	}

	if isForwardProxyMode(ctx.ProxyMode) && ctx.ForwarderProxy != nil {
		add(ctx.ForwarderProxy.ProxyServer)
	}
	add(ctx.DNS.CheckHost)
	for _, host := range ctx.PreflightHosts {
		add(host)
	}
	return hosts
}

// probePreflightHosts resolves each host once and collects every failure,
// rather than stopping at the first. Which names fail is the diagnosis: the
// proxy alone failing is a different problem from every name failing, and an
// operator who is told about one of three has to run the check again to learn
// the rest.
func probePreflightHosts(hosts []string) []error {
	var failures []error
	for _, host := range hosts {
		if err := dnsServiceOps.resolve(host); err != nil {
			failures = append(failures, err)
		}
	}
	return failures
}

// runPreflight probes the context's names, and on failure retries once with
// the resolvers handed back to DHCP.
//
// The DHCP attempt is a DIAGNOSIS, not a repair: it is always undone. If it
// succeeds, the operator learns that the network is fine and the configured
// resolvers are the problem - which is precisely the fact that turns an
// unswitchable machine into a one-line fix. Leaving the machine on DHCP
// instead would be a third state, neither the old context nor the new one,
// and switchContext has no way to reason about that.
func runPreflight(cfg Config, ctx SwitchContext) (preflightResult, error) {
	result := preflightResult{Hosts: preflightHosts(ctx)}
	if len(result.Hosts) == 0 {
		result.OK = true
		return result, nil
	}

	result.Failures = probePreflightHosts(result.Hosts)
	if len(result.Failures) == 0 {
		result.OK = true
		return result, nil
	}

	services, err := resolveNetworkServices(cfg)
	if err != nil {
		return result, fmt.Errorf("preflight: cannot list network services: %w", err)
	}

	restore, err := useDHCPResolvers(services)
	if err != nil {
		return result, err
	}
	result.UsedDHCP = true
	defer restore()

	dnsServiceOps.flush()
	result.Failures = probePreflightHosts(result.Hosts)
	result.OK = len(result.Failures) == 0
	return result, nil
}

// useDHCPResolvers points every network service at DHCP's resolvers and
// returns the function that puts the previous ones back.
//
// The restore is registered with a signal handler as well as being returned
// for a defer. A defer does not run when the process is killed, and the window
// here contains a multi-second DNS probe - more than long enough for an
// impatient Ctrl-C. Losing the restore would leave the machine's resolvers
// silently replaced by a diagnostic step, which is a worse outcome than the
// problem being diagnosed.
//
// Restoring is idempotent and safe to race: whichever of the two paths runs
// first wins, and the other returns immediately.
func useDHCPResolvers(services []string) (func(), error) {
	saved := make(map[string][]string, len(services))
	var applied []string

	restoreAll := func() {
		for _, svc := range applied {
			if err := dnsServiceOps.set(svc, saved[svc]); err != nil {
				// Loud, and not fatal: the remaining services still
				// have to be put back. A half-restored machine is
				// worse than a fully restored one with a warning.
				fmt.Printf("warning: could not restore DNS servers for %s: %v\n", svc, err)
			}
		}
	}

	var once sync.Once
	stop := make(chan struct{})
	restore := func() {
		once.Do(func() {
			close(stop)
			restoreAll()
		})
	}

	for _, svc := range services {
		current, err := dnsServiceOps.get(svc)
		if err != nil {
			restore()
			return nil, fmt.Errorf("preflight: %w", err)
		}
		saved[svc] = current
		// Recorded BEFORE the set, not after: a set that fails partway
		// may still have changed the service, and a restore that skips
		// it would leave exactly that service on DHCP.
		applied = append(applied, svc)
		if err := dnsServiceOps.set(svc, nil); err != nil {
			restore()
			return nil, fmt.Errorf("preflight: %w", err)
		}
	}

	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
	go func() {
		defer signal.Stop(signals)
		select {
		case <-signals:
			restore()
			// Re-raise with the handler removed so the process dies
			// the way the operator asked it to. Swallowing the signal
			// here would make Ctrl-C look like it did nothing.
			signal.Reset(os.Interrupt, syscall.SIGTERM)
			_ = syscall.Kill(os.Getpid(), syscall.SIGINT)
		case <-stop:
		}
	}()

	return restore, nil
}

// preflightError turns a failed check into the message the operator acts on.
// The DHCP outcome is the whole point of the check, so it decides the hint:
// the two cases have different fixes and nothing else distinguishes them.
func preflightError(name string, result preflightResult) error {
	var b strings.Builder
	fmt.Fprintf(&b, "preflight for context %s failed: cannot resolve %s", name, strings.Join(result.Hosts, ", "))
	for _, err := range result.Failures {
		fmt.Fprintf(&b, "\n  - %v", err)
	}
	if result.UsedDHCP {
		b.WriteString("\nhint: the names did not resolve with DHCP's resolvers either, so the problem is" +
			" upstream of DNS configuration - check the link itself (Wi-Fi/Ethernet, captive portal, VPN reachability).")
	} else {
		b.WriteString("\nhint: DHCP's resolvers were not tried.")
	}
	return errors.New(b.String())
}

// preflightDHCPWorkedError is returned when DHCP's resolvers DID resolve the
// names the configured ones could not. That is the diagnosis worth having, so
// it gets its own message rather than being folded into a generic failure.
func preflightDHCPWorkedError(name string, result preflightResult) error {
	return fmt.Errorf(
		"preflight for context %s failed: %s did not resolve with the configured resolvers,"+
			" but did resolve with DHCP's\n"+
			"hint: the network is fine and the DNS configuration is not. The resolvers have been put"+
			" back as they were. Fix the local resolver (AdGuard Home, `macswitcher observe`) or"+
			" switch to a context whose resolvers this network can reach - `sudo networksetup"+
			" -setdnsservers <service> Empty` is the manual escape hatch",
		name, strings.Join(result.Hosts, ", "),
	)
}

// preflight runs the check and translates its outcome into the one thing
// switchContext needs: nil, or an error the operator can act on.
//
// Note that a check which only passed via DHCP is a FAILURE here. The
// resolvers were put straight back, so the switch would hit the same dead end
// a second later - and stopping with the diagnosis in hand is more use than
// failing again with "getaddrinfo failed".
func preflight(cfg Config, ctx SwitchContext, name string) error {
	result, err := runPreflight(cfg, ctx)
	if err != nil {
		return err
	}
	switch {
	case result.OK && !result.UsedDHCP:
		return nil
	case result.OK:
		return preflightDHCPWorkedError(name, result)
	default:
		return preflightError(name, result)
	}
}
