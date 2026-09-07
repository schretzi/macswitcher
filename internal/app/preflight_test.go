package app

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

// fakeDNS is a description of a network, standing in for the machine.
type fakeDNS struct {
	// configured is what each service currently has; nil means DHCP.
	configured map[string][]string
	// resolvable lists the names that resolve with the CURRENTLY configured
	// resolvers, and dhcpResolvable those that resolve once a service is
	// handed back to DHCP. Keeping them apart is the whole point: the
	// difference between the two is what preflight exists to detect.
	resolvable     map[string]bool
	dhcpResolvable map[string]bool
	// setCalls records every set in order, so a test can assert that the
	// restore actually happened and with what.
	setCalls []dnsSetCall
	flushes  int
	// setErr, when non-nil, fails the set for that service.
	setErr map[string]error
	getErr map[string]error
}

type dnsSetCall struct {
	service   string
	resolvers []string
}

// inDHCPWindow reports whether the DHCP retry is currently open, keyed on the
// cache flush. preflight flushes exactly once, immediately after handing the
// services to DHCP and before probing again, so the flush count is a faithful
// marker of the window - and, unlike inspecting the configured resolvers, it
// does not get confused by a service that was already on DHCP to begin with.
func (f *fakeDNS) inDHCPWindow() bool { return f.flushes > 0 }

// stubDNSServiceOps replaces the only path from preflight to the machine.
// Every test that reaches preflight must call it; TestMain arms the real
// table with panics precisely so that forgetting is impossible to miss.
func stubDNSServiceOps(t *testing.T, fake *fakeDNS) {
	t.Helper()
	original := dnsServiceOps
	dnsServiceOps = dnsOps{
		get: func(service string) ([]string, error) {
			if err := fake.getErr[service]; err != nil {
				return nil, err
			}
			return fake.configured[service], nil
		},
		set: func(service string, resolvers []string) error {
			if err := fake.setErr[service]; err != nil {
				return err
			}
			fake.setCalls = append(fake.setCalls, dnsSetCall{service: service, resolvers: resolvers})
			fake.configured[service] = resolvers
			return nil
		},
		flush: func() { fake.flushes++ },
		resolve: func(host string) error {
			table := fake.resolvable
			if fake.inDHCPWindow() {
				table = fake.dhcpResolvable
			}
			if table[host] {
				return nil
			}
			return errors.New(host + " did not resolve")
		},
	}
	t.Cleanup(func() { dnsServiceOps = original })
}

func cfgWithServices(services ...string) Config {
	return Config{NetworkServices: services}
}

func TestPreflightHostsDerivesAndDeduplicates(t *testing.T) {
	ctx := SwitchContext{
		ProxyMode:      ProxyModeForward,
		ForwarderProxy: &ForwarderProxyConfig{ProxyServer: "proxy.corp.example"},
		DNS:            ContextDNSConfig{CheckHost: "intranet.corp.example"},
		PreflightHosts: []string{"vpn.corp.example", "PROXY.CORP.EXAMPLE", "  ", "vpn.corp.example"},
	}
	got := preflightHosts(ctx)
	want := []string{"proxy.corp.example", "intranet.corp.example", "vpn.corp.example"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("preflightHosts() = %v, want %v", got, want)
	}
}

// A VPN context's forward proxy hostname is an internal corporate DNS name -
// www-proxy.billa.co.at, reached only through the resolvers the tunnel itself
// brings up. Requiring it to resolve here, before the switch has done
// anything, made such a context unswitchable from any network that cannot
// reach it directly: office -> home-vpn failed preflight every time on a home
// network, even though the one name that matters before the tunnel exists -
// the VPN gateway - resolved fine. checkDNSResolution proves the proxy's name
// resolves once the tunnel and upstreams are in place; preflight must not
// duplicate that check before it is possible to pass.
func TestPreflightHostsOmitsForwardProxyForVPNContexts(t *testing.T) {
	ctx := SwitchContext{
		ProxyMode:      ProxyModeForward,
		ForwarderProxy: &ForwarderProxyConfig{ProxyServer: "www-proxy.billa.co.at"},
		PreflightHosts: []string{"puma.rewe-group.at"},
		Apps:           LifecycleConfig{Start: []string{"vpn", "kerberos_keep_alive"}},
	}
	got := preflightHosts(ctx)
	want := []string{"puma.rewe-group.at"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("preflightHosts() = %v, want %v (the forward proxy hostname must not be checked pre-tunnel)", got, want)
	}
}

// A context that names nothing must not invent a check. google.com is a fine
// default for "did the switch work" AFTER the fact, but as a precondition it
// would refuse to switch on any network that resolves only its intranet.
func TestPreflightHostsEmptyWhenNothingDeclared(t *testing.T) {
	if got := preflightHosts(SwitchContext{ProxyMode: ProxyModeOff}); len(got) != 0 {
		t.Fatalf("preflightHosts() = %v, want none", got)
	}
}

func TestPreflightPassesWithoutTouchingDNS(t *testing.T) {
	fake := &fakeDNS{
		configured: map[string][]string{"Wi-Fi": {"127.0.0.1"}},
		resolvable: map[string]bool{"vpn.corp.example": true},
	}
	stubDNSServiceOps(t, fake)

	ctx := SwitchContext{PreflightHosts: []string{"vpn.corp.example"}}
	if err := preflight(cfgWithServices("Wi-Fi"), ctx, "office"); err != nil {
		t.Fatalf("preflight() = %v, want nil", err)
	}
	if len(fake.setCalls) != 0 {
		t.Fatalf("preflight changed DNS on the good path: %v", fake.setCalls)
	}
	if fake.flushes != 0 {
		t.Fatalf("preflight flushed the DNS cache on the good path")
	}
}

// The case the whole feature exists for: the configured resolvers are broken,
// DHCP's are not. preflight must say so, and must put the resolvers back.
func TestPreflightReportsWhenDHCPResolvesAndConfiguredDoesNot(t *testing.T) {
	fake := &fakeDNS{
		configured:     map[string][]string{"Wi-Fi": {"127.0.0.1"}, "Ethernet": nil},
		resolvable:     map[string]bool{},
		dhcpResolvable: map[string]bool{"vpn.corp.example": true},
	}
	stubDNSServiceOps(t, fake)

	ctx := SwitchContext{PreflightHosts: []string{"vpn.corp.example"}}
	err := preflight(cfgWithServices("Wi-Fi", "Ethernet"), ctx, "office")
	if err == nil {
		t.Fatal("preflight() = nil, want an error naming the DHCP result")
	}
	if !strings.Contains(err.Error(), "did resolve with DHCP") {
		t.Fatalf("preflight() = %v, want the DHCP diagnosis", err)
	}
	assertRestored(t, fake, map[string][]string{"Wi-Fi": {"127.0.0.1"}, "Ethernet": nil})
}

// Both resolver sets failing means the link is the problem, not the DNS
// configuration - a different fix, so a different message.
func TestPreflightReportsWhenNeitherResolverSetWorks(t *testing.T) {
	fake := &fakeDNS{
		configured: map[string][]string{"Wi-Fi": {"127.0.0.1"}},
	}
	stubDNSServiceOps(t, fake)

	ctx := SwitchContext{PreflightHosts: []string{"vpn.corp.example"}}
	err := preflight(cfgWithServices("Wi-Fi"), ctx, "office")
	if err == nil {
		t.Fatal("preflight() = nil, want an error")
	}
	if strings.Contains(err.Error(), "did resolve with DHCP") {
		t.Fatalf("preflight() = %v, want the link-level hint", err)
	}
	if !strings.Contains(err.Error(), "upstream of DNS configuration") {
		t.Fatalf("preflight() = %v, want the link-level hint", err)
	}
	assertRestored(t, fake, map[string][]string{"Wi-Fi": {"127.0.0.1"}})
}

// Every failing name has to be reported. Being told about one of three means
// running the check again to learn the rest, at which point it has not done
// its job.
func TestPreflightReportsEveryFailingHost(t *testing.T) {
	// ok.example resolves under both resolver sets, so it is never a failure
	// and must not appear in the headline. bad-* fail under both.
	fake := &fakeDNS{
		configured:     map[string][]string{"Wi-Fi": nil},
		resolvable:     map[string]bool{"ok.example": true},
		dhcpResolvable: map[string]bool{"ok.example": true},
	}
	stubDNSServiceOps(t, fake)

	ctx := SwitchContext{PreflightHosts: []string{"ok.example", "bad-one.example", "bad-two.example"}}
	err := preflight(cfgWithServices("Wi-Fi"), ctx, "office")
	if err == nil {
		t.Fatal("preflight() = nil, want an error")
	}
	for _, host := range []string{"bad-one.example", "bad-two.example"} {
		if !strings.Contains(err.Error(), host) {
			t.Fatalf("preflight() = %v, does not mention %s", err, host)
		}
	}
	// And it must not accuse the name that resolved perfectly well. The
	// headline used to list every probed host, so a working VPN gateway was
	// reported as unresolvable alongside a genuinely broken proxy - which
	// sends the operator to look at the tunnel instead of the proxy.
	headline, _, _ := strings.Cut(err.Error(), "\n")
	accused, _, _ := strings.Cut(headline, " (")
	if strings.Contains(accused, "ok.example") {
		t.Fatalf("headline %q accuses ok.example, which resolved", headline)
	}
	if !strings.Contains(err.Error(), "ok.example did resolve") {
		t.Fatalf("preflight() = %v, want ok.example reported as resolving", err)
	}
}

// The DHCP diagnosis has to name the hosts the CONFIGURED resolvers could not
// handle. After a successful DHCP retry the last attempt has no failures at
// all, so a message built from it would name nobody.
func TestPreflightDHCPDiagnosisNamesOnlyTheConfiguredFailures(t *testing.T) {
	fake := &fakeDNS{
		configured:     map[string][]string{"Wi-Fi": {"127.0.0.1"}},
		resolvable:     map[string]bool{"ok.example": true},
		dhcpResolvable: map[string]bool{"ok.example": true, "broken.example": true},
	}
	stubDNSServiceOps(t, fake)

	ctx := SwitchContext{PreflightHosts: []string{"ok.example", "broken.example"}}
	err := preflight(cfgWithServices("Wi-Fi"), ctx, "office")
	if err == nil {
		t.Fatal("preflight() = nil, want the DHCP diagnosis")
	}
	if !strings.Contains(err.Error(), "broken.example did not resolve with the configured") {
		t.Fatalf("preflight() = %v, want broken.example named", err)
	}
	if strings.Contains(err.Error(), "ok.example did not resolve") {
		t.Fatalf("preflight() = %v, must not accuse ok.example", err)
	}
}

// A service that was on DHCP must go back to DHCP, not to a resolver called
// "There" - see noDNSServersPrefix.
func TestPreflightRestoresDHCPServicesAsDHCP(t *testing.T) {
	fake := &fakeDNS{
		configured:     map[string][]string{"Wi-Fi": {"127.0.0.1"}, "Ethernet": nil},
		dhcpResolvable: map[string]bool{"vpn.corp.example": true},
	}
	stubDNSServiceOps(t, fake)

	ctx := SwitchContext{PreflightHosts: []string{"vpn.corp.example"}}
	_ = preflight(cfgWithServices("Wi-Fi", "Ethernet"), ctx, "office")

	if got := fake.configured["Ethernet"]; len(got) != 0 {
		t.Fatalf("Ethernet restored to %v, want it back on DHCP", got)
	}
	for _, call := range fake.setCalls {
		if call.service == "Ethernet" && len(call.resolvers) == 1 && strings.HasPrefix(call.resolvers[0], "There") {
			t.Fatal("restore wrote networksetup's \"no servers\" sentence back as a resolver")
		}
	}
}

// A set that fails partway must still restore the services already changed.
// Recording a service as applied BEFORE the set is what makes that hold even
// when the failing set had already taken effect.
func TestPreflightRestoresAfterAPartialFailure(t *testing.T) {
	fake := &fakeDNS{
		configured: map[string][]string{"Wi-Fi": {"127.0.0.1"}, "Ethernet": {"10.0.0.1"}},
		setErr:     map[string]error{"Ethernet": errors.New("boom")},
	}
	stubDNSServiceOps(t, fake)

	ctx := SwitchContext{PreflightHosts: []string{"vpn.corp.example"}}
	if err := preflight(cfgWithServices("Wi-Fi", "Ethernet"), ctx, "office"); err == nil {
		t.Fatal("preflight() = nil, want the set error")
	}
	if got := fake.configured["Wi-Fi"]; !reflect.DeepEqual(got, []string{"127.0.0.1"}) {
		t.Fatalf("Wi-Fi left on %v, want its original resolvers back", got)
	}
}

func TestGetDNSServersTreatsTheNoServersSentenceAsDHCP(t *testing.T) {
	// The exact string networksetup prints, which parsed naively yields a
	// resolver named "There".
	const out = "There aren't any DNS Servers set on Wi-Fi."
	var resolvers []string
	for line := range strings.SplitSeq(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, noDNSServersPrefix) {
			continue
		}
		resolvers = append(resolvers, line)
	}
	if len(resolvers) != 0 {
		t.Fatalf("parsed %v, want none", resolvers)
	}
}

func TestDNSServerArgsSpellsDHCPAsEmpty(t *testing.T) {
	if got := dnsServerArgs(nil); !reflect.DeepEqual(got, []string{"Empty"}) {
		t.Fatalf("dnsServerArgs(nil) = %v, want [Empty]", got)
	}
	if got := dnsServerArgs([]string{"1.1.1.1"}); !reflect.DeepEqual(got, []string{"1.1.1.1"}) {
		t.Fatalf("dnsServerArgs = %v, want the resolvers unchanged", got)
	}
}

func assertRestored(t *testing.T, fake *fakeDNS, want map[string][]string) {
	t.Helper()
	for svc, resolvers := range want {
		if got := fake.configured[svc]; !reflect.DeepEqual(got, resolvers) {
			t.Fatalf("%s left on %v, want %v", svc, got, resolvers)
		}
	}
}
