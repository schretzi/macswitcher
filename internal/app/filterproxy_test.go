package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func filterProxyTestConfig() Config {
	return Config{
		CurrentContext: contextHome,
		LocalProxy: LocalProxyConfig{
			Host:    loopbackLocal,
			Port:    3128,
			NoProxy: []string{"localhost", loopbackLocal, loopbackIPv6, "kubernetes", "kiac"},
		},
		FilterProxy: FilterProxyConfig{
			Enabled:  true,
			Host:     loopbackLocal,
			Port:     defaultFilterProxyPort,
			FailOpen: true,
			Direct:   []string{".ts.net"},
		},
		Contexts: map[string]SwitchContext{
			contextHome: {ProxyMode: ProxyModeDirect},
			"work":      {ProxyMode: ProxyModeForward},
			"off":       {ProxyMode: ProxyModeOff},
		},
	}
}

func TestFilterProxyAppliesToDirectContextsOnly(t *testing.T) {
	t.Parallel()

	cfg := filterProxyTestConfig()
	for name, want := range map[string]bool{contextHome: true, "work": false, "off": false} {
		if got := filterProxyAppliesTo(cfg, cfg.Contexts[name]); got != want {
			t.Errorf("filterProxyAppliesTo(%q) = %v, want %v", name, got, want)
		}
	}

	// A forward context reaches the internet through the corporate proxy,
	// which filters already; disabling the block must take it out everywhere.
	cfg.FilterProxy.Enabled = false
	if filterProxyAppliesTo(cfg, cfg.Contexts[contextHome]) {
		t.Error("filter_proxy disabled still applies to a direct context")
	}
}

func TestRenderFilterPACRoutesLocalTrafficDirect(t *testing.T) {
	t.Parallel()

	pac := renderFilterPAC(filterProxyTestConfig())

	// The no_proxy list and filter_proxy.direct both have to reach the PAC -
	// the whole reason macswitcher generates it instead of pointing at a
	// hand-written file is that these lists must not drift apart.
	for _, want := range []string{"kubernetes", "kiac", ".ts.net"} {
		if !strings.Contains(pac, want) {
			t.Errorf("generated PAC does not mention %q:\n%s", want, pac)
		}
	}
	if !strings.Contains(pac, `return "PROXY 127.0.0.1:8118; DIRECT"`) {
		t.Errorf("fail-open PAC lacks the DIRECT fallback:\n%s", pac)
	}
	// dnsResolve() in a PAC costs a lookup on every request, through the very
	// resolver this tool reconfigures on a switch.
	if strings.Contains(pac, "dnsResolve") {
		t.Errorf("generated PAC calls dnsResolve():\n%s", pac)
	}
}

func TestRenderFilterPACFailClosedHasNoFallback(t *testing.T) {
	t.Parallel()

	cfg := filterProxyTestConfig()
	cfg.FilterProxy.FailOpen = false
	pac := renderFilterPAC(cfg)

	if strings.Contains(pac, "; DIRECT\"") {
		t.Errorf("fail-closed PAC still offers a DIRECT fallback:\n%s", pac)
	}
	if !strings.Contains(pac, `return "PROXY 127.0.0.1:8118"`) {
		t.Errorf("fail-closed PAC does not return the proxy:\n%s", pac)
	}
}

// The private-range tests are numeric in the generated PAC because glob
// patterns get the boundaries wrong: "100.1??.*" swallows 100.128.0.0/9,
// which is public, and a "172.*" style rule swallows 172.32.
func TestRenderFilterPACComparesIPv4Numerically(t *testing.T) {
	t.Parallel()

	pac := renderFilterPAC(filterProxyTestConfig())
	for _, want := range []string{
		"a === 172 && b >= 16 && b <= 31",
		"a === 100 && b >= 64 && b <= 127",
	} {
		if !strings.Contains(pac, want) {
			t.Errorf("generated PAC lost the numeric range check %q:\n%s", want, pac)
		}
	}
}

func TestFilterProxyDirectHostsDeduplicates(t *testing.T) {
	t.Parallel()

	cfg := filterProxyTestConfig()
	cfg.FilterProxy.Direct = []string{"kubernetes", ".ts.net", "kubernetes"}
	hosts := filterProxyDirectHosts(cfg)

	seen := map[string]int{}
	for _, h := range hosts {
		seen[h]++
	}
	for h, n := range seen {
		if n > 1 {
			t.Errorf("host %q appears %d times in %#v", h, n, hosts)
		}
	}
	// Loopback is handled numerically further down the generated PAC.
	for _, unwanted := range []string{loopbackLocal, loopbackIPv6} {
		if seen[unwanted] > 0 {
			t.Errorf("%q should not be emitted as a named direct host: %#v", unwanted, hosts)
		}
	}
}

func TestWriteFilterPACIsAtomicAndReadable(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	cfg := filterProxyTestConfig()
	cfg.FilterProxy.PACFile = filepath.Join(dir, "nested", "filter.pac")

	path, err := writeFilterPAC(cfg)
	if err != nil {
		t.Fatalf("writeFilterPAC() error = %v", err)
	}
	body, err := os.ReadFile(path) // #nosec G304 -- path is the temp file this test just wrote
	if err != nil {
		t.Fatalf("reading generated PAC: %v", err)
	}
	if !strings.Contains(string(body), "FindProxyForURL") {
		t.Errorf("generated PAC has no entry point:\n%s", body)
	}
	// alpaca reads it as an unprivileged process; 0600 would break it.
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Mode().Perm() != 0o644 {
		t.Errorf("PAC mode = %v, want 0644", info.Mode().Perm())
	}
	// No temporary files left behind next to it.
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".filter-") {
			t.Errorf("temporary file %q left behind", e.Name())
		}
	}
}

func TestBuildProxyCommandInjectsGeneratedPAC(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	cfg := filterProxyTestConfig()
	cfg.FilterProxy.PACFile = filepath.Join(dir, "filter.pac")
	cfg.Alpaca = AlpacaConfig{
		Enabled: true,
		Command: []string{appAlpaca, "-l", placeholderLocalHost, "-p", placeholderLocalPort, "-C", placeholderPACFile},
	}

	got, err := buildProxyCommand(cfg, cfg.Alpaca)
	if err != nil {
		t.Fatalf("buildProxyCommand() error = %v", err)
	}
	joined := strings.Join(got, " ")
	if !strings.Contains(joined, "-C file://"+cfg.FilterProxy.PACFile) {
		t.Fatalf("alpaca command does not point at the generated PAC: %v", got)
	}

	// Disabled, the -C pair has to disappear again rather than survive as an
	// empty flag - alpaca treats a bare -C as a missing argument.
	cfg.FilterProxy.Enabled = false
	got, err = buildProxyCommand(cfg, cfg.Alpaca)
	if err != nil {
		t.Fatalf("buildProxyCommand() error = %v", err)
	}
	for _, arg := range got {
		if arg == "-C" {
			t.Fatalf("-C survived with filter_proxy disabled: %v", got)
		}
	}
}

func TestFilterProxyStatusLineReportsTheSilentCase(t *testing.T) {
	t.Parallel()

	cfg := filterProxyTestConfig()
	// Nothing is listening on this port in a test run, which is precisely the
	// state the fail-open PAC hides and the status line exists to surface.
	cfg.FilterProxy.Port = 1 // privileged and unused; nothing will answer
	line := filterProxyStatusLine(cfg, cfg.Contexts[contextHome])
	if !strings.Contains(line, "NOT LISTENING") || !strings.Contains(line, "unfiltered") {
		t.Errorf("status line does not report the fail-open gap: %q", line)
	}

	forward := filterProxyStatusLine(cfg, cfg.Contexts["work"])
	if !strings.Contains(forward, "not in path") {
		t.Errorf("forward context should report the filter as out of path: %q", forward)
	}

	cfg.FilterProxy.Enabled = false
	if got := filterProxyStatusLine(cfg, cfg.Contexts[contextHome]); got != "disabled" {
		t.Errorf("disabled status line = %q", got)
	}
}

func TestCommandNamesPACMatchesTheFlagNotAnySubstring(t *testing.T) {
	t.Parallel()

	if !commandNamesPAC([]string{appAlpaca, "-C", "file:///tmp/x.pac"}) {
		t.Error("did not recognise an explicit -C")
	}
	// commandUsesToken would call this a match, because the path contains -C.
	if commandNamesPAC([]string{appAlpaca, "-l", "/Users/me/proxy-Config/alpaca"}) {
		t.Error("matched -C inside a path")
	}
}
