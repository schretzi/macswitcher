package app

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestDscacheutilOutputHasAddress(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		out  string
		want bool
	}{
		{
			name: "ipv4 address present",
			out:  "name: google.com\nip_address: 142.251.110.113\n",
			want: true,
		},
		{
			name: "ipv6 address present",
			out:  "name: google.com\nipv6_address: 2a00:1450:4001:c15::8b\n",
			want: true,
		},
		{
			name: "no address (unresolvable host)",
			out:  "",
			want: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := dscacheutilOutputHasAddress(tt.out); got != tt.want {
				t.Fatalf("dscacheutilOutputHasAddress(%q) = %v, want %v", tt.out, got, tt.want)
			}
		})
	}
}

func TestCheckDNSResolutionForwardModeRequiresProxyServer(t *testing.T) {
	t.Parallel()

	ctx := SwitchContext{
		ProxyMode:      ProxyModeForward,
		ForwarderProxy: &ForwarderProxyConfig{},
	}
	err := checkDNSResolution(ctx)
	if err == nil {
		t.Fatal("expected an error when forward mode has no proxy_server configured")
	}
}

// Which name proves DNS works is the whole point of dns.check_host: a network
// whose resolvers serve the intranet only cannot answer either default, so a
// context that sets it must have it used in preference to both.
func TestDNSCheckHost(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		ctx     SwitchContext
		want    string
		wantErr bool
	}{
		{
			name: "defaults to google.com",
			ctx:  SwitchContext{ProxyMode: ProxyModeDirect},
			want: "google.com",
		},
		{
			name: "forward mode uses the proxy's own hostname",
			ctx: SwitchContext{
				ProxyMode:      ProxyModeForward,
				ForwarderProxy: &ForwarderProxyConfig{ProxyServer: "proxy.example.com"},
			},
			want: "proxy.example.com",
		},
		{
			name: "check_host overrides the default",
			ctx: SwitchContext{
				ProxyMode: ProxyModeOff,
				DNS:       ContextDNSConfig{CheckHost: "intranet.example.com"},
			},
			want: "intranet.example.com",
		},
		{
			name: "check_host also overrides the forward proxy's hostname",
			ctx: SwitchContext{
				ProxyMode:      ProxyModeForward,
				ForwarderProxy: &ForwarderProxyConfig{ProxyServer: "proxy.example.com"},
				DNS:            ContextDNSConfig{CheckHost: "intranet.example.com"},
			},
			want: "intranet.example.com",
		},
		{
			name: "surrounding whitespace is not a value",
			ctx: SwitchContext{
				ProxyMode: ProxyModeDirect,
				DNS:       ContextDNSConfig{CheckHost: "   "},
			},
			want: "google.com",
		},
		{
			name: "forward mode without a proxy_server is an error",
			ctx: SwitchContext{
				ProxyMode:      ProxyModeForward,
				ForwarderProxy: &ForwarderProxyConfig{},
			},
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := dnsCheckHost(tt.ctx)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("dnsCheckHost() = %q, want an error", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("dnsCheckHost(): %v", err)
			}
			if got != tt.want {
				t.Errorf("dnsCheckHost() = %q, want %q", got, tt.want)
			}
		})
	}
}

// checkDNSResolution retries so that a forward context's VPN has time to bring
// its resolvers up. The retry must still give up: a name that never resolves
// has to end the switch rather than spin forever.
func TestCheckDNSResolutionGivesUpAfterTheTimeout(t *testing.T) {
	origTimeout, origPoll := dnsResolveTimeout, dnsResolvePollInterval
	dnsResolveTimeout, dnsResolvePollInterval = 50*time.Millisecond, time.Millisecond
	t.Cleanup(func() {
		dnsResolveTimeout, dnsResolvePollInterval = origTimeout, origPoll
	})

	// .invalid is reserved by RFC 2606 and cannot resolve anywhere.
	ctx := SwitchContext{
		ProxyMode:      ProxyModeForward,
		ForwarderProxy: &ForwarderProxyConfig{ProxyServer: "macswitcher-test.invalid"},
	}
	done := make(chan error, 1)
	go func() { done <- checkDNSResolution(ctx) }()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected an error for a name that cannot resolve")
		}
	case <-time.After(30 * time.Second):
		t.Fatal("checkDNSResolution did not give up; the retry loop has no exit")
	}
}

// The per-domain "[/zone/]addr" upstreams in AdGuard Home's upstream file come
// contributed by an overlay (the work overlay's intranet zones), not from
// macswitcher. A context switch rewrites the default upstreams and must carry
// those over untouched - losing them takes the intranet off the air in a way
// that only shows up on the next intranet lookup.
func TestWriteAdGuardUpstreamsPreservesPerDomainEntries(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "upstreams.conf")
	existing := strings.Join([]string{
		"# per-domain upstreams, not macswitcher's",
		"9.9.9.9",
		"8.8.8.8",
		"",
		"# from corp.conf",
		"[/corp.example/]10.0.0.106",
		"[/intranet.example/]10.0.0.107",
		"",
	}, "\n")
	if err := os.WriteFile(path, []byte(existing), 0o644); err != nil {
		t.Fatalf("seed upstreams: %v", err)
	}

	cfg := Config{AdGuard: AdGuardConfig{UpstreamsFile: path}}
	if err := writeAdGuardUpstreams(cfg, []string{"10.0.0.149", " ", "10.0.0.70"}, "work"); err != nil {
		t.Fatalf("writeAdGuardUpstreams() error = %v", err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	defaults, specific, err := currentAdGuardUpstreams(path)
	if err != nil {
		t.Fatalf("currentAdGuardUpstreams() error = %v", err)
	}

	if want := []string{"10.0.0.149", "10.0.0.70"}; !slices.Equal(defaults, want) {
		t.Errorf("defaults = %v, want %v", defaults, want)
	}
	if want := []string{"[/corp.example/]10.0.0.106", "[/intranet.example/]10.0.0.107"}; !slices.Equal(specific, want) {
		t.Errorf("per-domain = %v, want %v", specific, want)
	}
	// The previous network's resolvers must be gone, not merely appended to.
	if strings.Contains(string(got), "9.9.9.9") {
		t.Errorf("stale default upstream 9.9.9.9 survived the rewrite:\n%s", got)
	}
	// AdGuard Home rejects a line with a trailing comment, so every comment
	// has to sit on a line of its own.
	for line := range strings.SplitSeq(string(got), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.Contains(line, "#") {
			t.Errorf("upstream line carries a trailing comment, which AdGuard Home rejects: %q", line)
		}
	}
}

// An empty upstreams_file means the machine has not opted into the trial.
func TestWriteAdGuardUpstreamsRequiresConfiguredPath(t *testing.T) {
	t.Parallel()

	if err := writeAdGuardUpstreams(Config{}, []string{"9.9.9.9"}, "home"); err == nil {
		t.Fatal("expected an error when adguard.upstreams_file is empty")
	}
}
