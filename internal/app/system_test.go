package app

import (
	"os"
	"path/filepath"
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
