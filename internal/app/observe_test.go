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

// TestKerberosDetailFallsBackToKeepAlive covers the reported bug: a context
// authenticating to its proxy with a Keychain password has no
// forwarder_proxy.ticket_file, and observe reported "not configured" while
// KerberosKeepAlive was maintaining a perfectly valid ticket.
func TestKerberosDetailFallsBackToKeepAlive(t *testing.T) {
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
	if !contains(lines, "kerberoskeepalive ccache_path") {
		t.Errorf("kerberosDetail() did not name its source: %v", lines)
	}
}

// TestKerberosDetailPrefersContextTicketFile keeps the fallback from taking
// over when the context does name a ticket: that file is the one the proxy
// actually authenticates with, so it is what the screen must report on.
func TestKerberosDetailPrefersContextTicketFile(t *testing.T) {
	writeKeepAliveConfig(t, "profiles:\n  - name: corp\n    ccache_path: /tmp/keepalive-ccache\n")
	cfg := Config{
		CurrentContext: "ctx",
		Contexts: map[string]SwitchContext{
			"ctx": {ForwarderProxy: &ForwarderProxyConfig{TicketFile: "/tmp/context-ccache"}},
		},
	}
	lines := kerberosDetail(cfg)
	if !contains(lines, "/tmp/context-ccache") {
		t.Errorf("kerberosDetail() did not use the context ticket_file: %v", lines)
	}
	if contains(lines, "/tmp/keepalive-ccache") {
		t.Errorf("kerberosDetail() used the fallback despite a context ticket_file: %v", lines)
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
