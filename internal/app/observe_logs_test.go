package app

import (
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestDeriveMainLogPath(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   string
		want string
	}{
		{name: "conventional stderr path", in: "/Users/x/Library/Logs/omt.err.log", want: "/Users/x/Library/Logs/omt.log"},
		{name: "name containing err", in: "/Users/x/Library/Logs/err.err.log", want: "/Users/x/Library/Logs/err.log"},
		{name: "not the convention", in: "/var/log/system.log", want: ""},
		{name: "empty", in: "", want: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := deriveMainLogPath(tt.in); got != tt.want {
				t.Fatalf("deriveMainLogPath(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

// writePlist renders an XML plist holding the given keys and returns its path.
func writePlist(t *testing.T, keys map[string]string) string {
	t.Helper()

	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>` + "\n<plist version=\"1.0\">\n<dict>\n")
	for _, key := range slices.Sorted(maps.Keys(keys)) {
		b.WriteString("  <key>" + key + "</key>\n  <string>" + keys[key] + "</string>\n")
	}
	b.WriteString("</dict>\n</plist>\n")

	path := filepath.Join(t.TempDir(), "com.example.daemon.plist")
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		t.Fatalf("writing plist: %v", err)
	}
	return path
}

func TestPlistString(t *testing.T) {
	t.Parallel()

	if _, err := exec.LookPath("plutil"); err != nil {
		t.Skip("plutil not available")
	}
	path := writePlist(t, map[string]string{
		"Label":             "com.example.daemon",
		"StandardErrorPath": "/Users/x/Library/Logs/daemon.err.log",
	})

	if got := plistString(path, "StandardErrorPath"); got != "/Users/x/Library/Logs/daemon.err.log" {
		t.Fatalf("plistString(StandardErrorPath) = %q", got)
	}
	if got := plistString(path, "StandardOutPath"); got != "" {
		t.Fatalf("plistString(StandardOutPath) = %q, want %q for a key the plist omits", got, "")
	}
	if got := plistString(filepath.Join(t.TempDir(), "missing.plist"), "Label"); got != "" {
		t.Fatalf("plistString(missing plist) = %q, want %q", got, "")
	}
}

func TestReadLogTail(t *testing.T) {
	t.Parallel()

	t.Run("short file is returned whole", func(t *testing.T) {
		t.Parallel()
		path := filepath.Join(t.TempDir(), "short.log")
		if err := os.WriteFile(path, []byte("first\nsecond\nthird\n"), 0o600); err != nil {
			t.Fatalf("writing log: %v", err)
		}
		got, err := readLogTail(path)
		if err != nil {
			t.Fatalf("readLogTail() error = %v", err)
		}
		want := []string{"first", "second", "third"}
		if !slices.Equal(got, want) {
			t.Fatalf("readLogTail() = %v, want %v", got, want)
		}
	})

	t.Run("caps at tailLines", func(t *testing.T) {
		t.Parallel()
		var b strings.Builder
		for i := range tailLines + 500 {
			b.WriteString(strings.Repeat("x", 20))
			b.WriteString(" line ")
			b.WriteString(string(rune('a' + i%26)))
			b.WriteString("\n")
		}
		path := filepath.Join(t.TempDir(), "long.log")
		if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
			t.Fatalf("writing log: %v", err)
		}
		got, err := readLogTail(path)
		if err != nil {
			t.Fatalf("readLogTail() error = %v", err)
		}
		if len(got) != tailLines {
			t.Fatalf("readLogTail() returned %d lines, want %d", len(got), tailLines)
		}
	})

	t.Run("drops the partial line a mid-file seek lands on", func(t *testing.T) {
		t.Parallel()
		// One line comfortably larger than tailBytes, followed by two whole
		// ones: the seek lands inside the first, which must not be reported.
		content := strings.Repeat("y", tailBytes+100) + "\nsecond\nthird\n"
		path := filepath.Join(t.TempDir(), "big.log")
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatalf("writing log: %v", err)
		}
		got, err := readLogTail(path)
		if err != nil {
			t.Fatalf("readLogTail() error = %v", err)
		}
		want := []string{"second", "third"}
		if !slices.Equal(got, want) {
			t.Fatalf("readLogTail() = %v, want %v", got, want)
		}
	})

	t.Run("missing file is an error", func(t *testing.T) {
		t.Parallel()
		if _, err := readLogTail(filepath.Join(t.TempDir(), "absent.log")); err == nil {
			t.Fatal("readLogTail() error = nil, want an error for a missing log")
		}
	})
}

func TestDaemonLogSourcesUnredirectedDaemon(t *testing.T) {
	t.Parallel()

	// A row with no label is not configured, so there is nothing to look at.
	if got := daemonLogSources(daemonRow{name: "vpn", kind: daemonKindVPN}); got != nil {
		t.Fatalf("daemonLogSources(unconfigured) = %v, want nil", got)
	}
	// A label with no plist installed likewise resolves to nothing, rather
	// than to a guessed path under ~/Library/Logs.
	row := daemonRow{name: "vpn", label: "com.example.definitely-not-installed", scope: daemonScopeUser, kind: daemonKindVPN}
	if got := daemonLogSources(row); len(got) != 0 {
		t.Fatalf("daemonLogSources(missing plist) = %v, want none", got)
	}
}

func TestLogModalSetContentHoldsPosition(t *testing.T) {
	t.Parallel()

	lines := make([]string, 100)
	for i := range lines {
		lines[i] = "line"
	}

	t.Run("follow jumps to the newest line", func(t *testing.T) {
		t.Parallel()
		l := logModal{viewport: newLogViewport(40, 10), follow: true}
		l.setContent(lines)
		if !l.viewport.AtBottom() {
			t.Fatal("setContent() left the viewport off the bottom while following")
		}
	})

	t.Run("scrolled back keeps its offset", func(t *testing.T) {
		t.Parallel()
		l := logModal{viewport: newLogViewport(40, 10)}
		l.setContent(lines)
		l.viewport.SetYOffset(20)
		l.setContent(lines)
		if got := l.viewport.YOffset; got != 20 {
			t.Fatalf("setContent() moved the viewport to offset %d, want it held at 20", got)
		}
	})
}
