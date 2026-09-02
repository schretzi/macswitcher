package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeKeepAliveConfig drops a kerberoskeepalive config into a temporary HOME
// and points the process at it.
func writeKeepAliveConfig(t *testing.T, body string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	if body == "" {
		return
	}
	dir := filepath.Join(home, ".config", "kerberoskeepalive")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(body), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
}

func TestKerberosCcacheFromKeepAlive(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
	}{
		{
			name: "first profile with a ccache_path wins",
			body: "profiles:\n  - name: a\n    ccache_path: /tmp/a\n  - name: b\n    ccache_path: /tmp/b\n",
			want: "/tmp/a",
		},
		{
			// A profile may legitimately omit ccache_path and use the
			// system default; skipping it finds the one that does name a
			// file rather than returning empty.
			name: "profiles without a ccache_path are skipped",
			body: "profiles:\n  - name: a\n  - name: b\n    ccache_path: /tmp/b\n",
			want: "/tmp/b",
		},
		{
			name: "no profiles yields no path",
			body: "profiles: []\n",
			want: "",
		},
		{
			name: "missing config yields no path",
			body: "",
			want: "",
		},
		{
			// Must not panic or propagate: observe is a status screen, and a
			// broken foreign config is something to report, not crash on.
			name: "malformed yaml yields no path",
			body: "profiles: [oh dear\n",
			want: "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			writeKeepAliveConfig(t, tt.body)
			if got := kerberosCcacheFromKeepAlive(); got != tt.want {
				t.Errorf("kerberosCcacheFromKeepAlive() = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestKerberosDetailUsesKeepAliveCcache covers the reported bug: observe read
// the ticket location from forwarder_proxy.ticket_file, which is a proxy
// credential rather than a ticket location, and reported "not configured"
// while KerberosKeepAlive was maintaining a perfectly valid ticket.
func TestKerberosDetailUsesKeepAliveCcache(t *testing.T) {
	writeKeepAliveConfig(t, "profiles:\n  - name: corp\n    ccache_path: /tmp/does-not-exist-ccache\n")
	cfg := Config{
		CurrentContext: "ctx",
		Contexts: map[string]SwitchContext{
			"ctx": {ForwarderProxy: &ForwarderProxyConfig{Username: "u"}},
		},
	}
	lines := kerberosDetail(cfg)
	if len(lines) == 0 {
		t.Fatal("kerberosDetail() returned no lines")
	}
	if !contains(lines, "/tmp/does-not-exist-ccache") {
		t.Errorf("kerberosDetail() did not mention the keepalive ccache: %v", lines)
	}
	// The file does not exist, so the ticket is invalid - and that must be
	// reported as survivable, since alpaca still has Basic to fall back on.
	if !contains(lines, "falls back to Basic") {
		t.Errorf("kerberosDetail() did not mention the Basic fallback: %v", lines)
	}
}

// contains reports whether any line mentions substr.
func contains(lines []string, substr string) bool {
	for _, l := range lines {
		if strings.Contains(l, substr) {
			return true
		}
	}
	return false
}
