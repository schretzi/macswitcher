package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
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
