package app

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func readProxyRC(t *testing.T) string {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(os.Getenv("HOME"), ".zsh", "rcs", "proxy"))
	if err != nil {
		t.Fatalf("reading proxy rc: %v", err)
	}
	return string(body)
}

// PROXY_STATE is what the Starship prompt shows, and "on" alone cannot say
// whether what is on also filters. Not parallel: it writes under $HOME.
func TestUpdateZshProxyReportsFilteredState(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	if err := updateZshProxy("http://127.0.0.1:3128", "localhost", true, "127.0.0.1:8118"); err != nil {
		t.Fatalf("updateZshProxy() error = %v", err)
	}
	rc := readProxyRC(t)
	if !strings.Contains(rc, `export PROXY_STATE="filtered"`) {
		t.Errorf("proxy on with a filter should read filtered:\n%s", rc)
	}
	if !strings.Contains(rc, `export PROXY_FILTER="127.0.0.1:8118"`) {
		t.Errorf("PROXY_FILTER does not name the filter:\n%s", rc)
	}
}

func TestUpdateZshProxyWithoutFilterStaysOn(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	if err := updateZshProxy("http://127.0.0.1:3128", "localhost", true, ""); err != nil {
		t.Fatalf("updateZshProxy() error = %v", err)
	}
	rc := readProxyRC(t)
	if !strings.Contains(rc, `export PROXY_STATE="on"`) {
		t.Errorf("proxy on without a filter should read on:\n%s", rc)
	}
	if !strings.Contains(rc, `export PROXY_FILTER=""`) {
		t.Errorf("PROXY_FILTER should be empty:\n%s", rc)
	}
}

// The proxy being off outranks everything: a stale filter address here would
// have the prompt claim filtering while nothing is proxied at all.
func TestUpdateZshProxyOffClearsTheFilter(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	if err := updateZshProxy("", "localhost", false, "127.0.0.1:8118"); err != nil {
		t.Fatalf("updateZshProxy() error = %v", err)
	}
	rc := readProxyRC(t)
	if !strings.Contains(rc, `export PROXY_STATE="off"`) {
		t.Errorf("disabled proxy should read off:\n%s", rc)
	}
	if !strings.Contains(rc, `export PROXY_FILTER=""`) {
		t.Errorf("disabled proxy should not name a filter:\n%s", rc)
	}
}

// waitForVPN is only meaningful when a context names the tunnel's interface,
// and it must not stall a switch that has no VPN to wait for.
func TestWaitForVPNReturnsWithoutAnInterfaceConfigured(t *testing.T) {
	t.Parallel()

	done := make(chan struct{})
	go func() {
		waitForVPN(Config{})
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("waitForVPN blocked although no VPN interface is configured")
	}
}

// A tunnel that never appears must not hold a switch open forever: waitForVPN
// warns and lets the switch carry on to checkDNSResolution, which is what
// actually decides whether it worked.
func TestWaitForVPNGivesUpOnAnInterfaceThatNeverAppears(t *testing.T) {
	origTimeout, origPoll := vpnUpTimeout, vpnUpPollInterval
	vpnUpTimeout, vpnUpPollInterval = 50*time.Millisecond, time.Millisecond
	t.Cleanup(func() {
		vpnUpTimeout, vpnUpPollInterval = origTimeout, origPoll
	})

	cfg := Config{}
	cfg.Daemons.VPN.Interface = "utun-macswitcher-test-absent"

	done := make(chan struct{})
	go func() {
		waitForVPN(cfg)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("waitForVPN did not give up on an interface that never appears")
	}
}

func TestUpdateLaunchdProxyPublishesBothCases(t *testing.T) {
	var got [][]string
	orig := runLaunchctl
	runLaunchctl = func(args ...string) error {
		got = append(got, args)
		return nil
	}
	t.Cleanup(func() { runLaunchctl = orig })

	if err := updateLaunchdProxy("http://127.0.0.1:3128", "localhost,kubernetes", true); err != nil {
		t.Fatalf("updateLaunchdProxy: %v", err)
	}

	want := [][]string{
		{"setenv", "HTTP_PROXY", "http://127.0.0.1:3128"},
		{"setenv", "NO_PROXY", "localhost,kubernetes"},
		{"setenv", "HTTPS_PROXY", "http://127.0.0.1:3128"},
		{"setenv", "http_proxy", "http://127.0.0.1:3128"},
		{"setenv", "no_proxy", "localhost,kubernetes"},
		{"setenv", "https_proxy", "http://127.0.0.1:3128"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("launchctl calls:\ngot  %v\nwant %v", got, want)
	}
}

func TestUpdateLaunchdProxyUnsetsEveryVariable(t *testing.T) {
	var got []string
	orig := runLaunchctl
	runLaunchctl = func(args ...string) error {
		if args[0] != "unsetenv" {
			t.Errorf("expected unsetenv, got %q", args[0])
		}
		got = append(got, args[1])
		return nil
	}
	t.Cleanup(func() { runLaunchctl = orig })

	if err := updateLaunchdProxy("", "", false); err != nil {
		t.Fatalf("updateLaunchdProxy: %v", err)
	}

	want := []string{"HTTP_PROXY", "NO_PROXY", "HTTPS_PROXY", "http_proxy", "no_proxy", "https_proxy"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("unset variables:\ngot  %v\nwant %v", got, want)
	}
}

// A restart_agents entry naming something absent from `applications` must warn
// rather than abort: the proxy is already applied by this point, and failing
// the switch would misreport a network change that in fact succeeded.
func TestRestartProxyConsumersSkipsUnknownAgent(t *testing.T) {
	orig := runCommandListFn
	var ran [][]string
	runCommandListFn = func(cmd []string) error {
		ran = append(ran, cmd)
		return nil
	}
	t.Cleanup(func() { runCommandListFn = orig })

	cfg := Config{
		LocalProxy: LocalProxyConfig{RestartAgents: []string{"tunneling", "nonexistent", ""}},
		Applications: map[string]ApplicationCommands{
			"tunneling": {Restart: []string{"tunneling", "service", "restart"}},
		},
	}
	restartProxyConsumers(cfg)

	want := [][]string{{"tunneling", "service", "restart"}}
	if !reflect.DeepEqual(ran, want) {
		t.Fatalf("commands run:\ngot  %v\nwant %v", ran, want)
	}
}
