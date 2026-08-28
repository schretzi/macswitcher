package app

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestExpandTilde(t *testing.T) {
	t.Parallel()

	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("os.UserHomeDir() error = %v", err)
	}

	tests := []struct {
		name string
		in   string
		want string
	}{
		{name: "tilde prefix", in: "~/Library/Caches/tickets/work.krb5cc", want: filepath.Join(home, "Library/Caches/tickets/work.krb5cc")},
		{name: "absolute path unchanged", in: "/tmp/work.krb5cc", want: "/tmp/work.krb5cc"},
		{name: "trims whitespace", in: "  /tmp/work.krb5cc  ", want: "/tmp/work.krb5cc"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := expandTilde(tt.in); got != tt.want {
				t.Fatalf("expandTilde(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestLaunchAgentPlistPath(t *testing.T) {
	t.Parallel()

	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("os.UserHomeDir() error = %v", err)
	}
	got, err := agentPlistPath(daemonScopeUser, "com.example.daemon")
	if err != nil {
		t.Fatalf("agentPlistPath() error = %v", err)
	}
	want := filepath.Join(home, "Library", "LaunchAgents", "com.example.daemon.plist")
	if got != want {
		t.Fatalf("agentPlistPath() = %q, want %q", got, want)
	}
}

func TestAgentPlistPathSystemScope(t *testing.T) {
	t.Parallel()

	got, err := agentPlistPath(daemonScopeSystem, "net.unbound")
	if err != nil {
		t.Fatalf("agentPlistPath() error = %v", err)
	}
	want := filepath.Join("/Library", "LaunchDaemons", "net.unbound.plist")
	if got != want {
		t.Fatalf("agentPlistPath() = %q, want %q", got, want)
	}
}

func TestInspectDaemonNotConfigured(t *testing.T) {
	t.Parallel()

	status := inspectDaemon("", daemonScopeUser)
	if status.Err == nil {
		t.Fatal("expected an error for an empty label")
	}
	if status.Installed || status.Loaded || status.Running {
		t.Fatalf("expected a zero-value status, got %#v", status)
	}
}

func TestInspectDaemonUnknownLabel(t *testing.T) {
	t.Parallel()

	status := inspectDaemon("com.schretzi.test-daemon-that-does-not-exist", daemonScopeUser)
	if status.Err != nil {
		t.Fatalf("unexpected error: %v", status.Err)
	}
	if status.Installed {
		t.Fatal("expected Installed = false for a label with no plist")
	}
	if status.Loaded || status.Running {
		t.Fatalf("expected an unloaded status, got %#v", status)
	}
}

func TestNormalizeDaemonScope(t *testing.T) {
	t.Parallel()

	tests := []struct {
		in   string
		want string
	}{
		{in: "", want: daemonScopeUser},
		{in: "user", want: daemonScopeUser},
		{in: "System", want: daemonScopeSystem},
		{in: "  system  ", want: daemonScopeSystem},
		{in: "bogus", want: daemonScopeUser},
	}
	for _, tt := range tests {
		if got := normalizeDaemonScope(tt.in); got != tt.want {
			t.Fatalf("normalizeDaemonScope(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestKerberosTicketStatusNotConfigured(t *testing.T) {
	t.Parallel()

	valid, summary, _ := kerberosTicketStatus("")
	if valid {
		t.Fatal("expected valid = false for an empty ticket_file")
	}
	if summary != "not configured" {
		t.Fatalf("summary = %q, want %q", summary, "not configured")
	}
}

func TestKerberosTicketStatusMissingFile(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	valid, summary, detail := kerberosTicketStatus(filepath.Join(dir, "missing.krb5cc"))
	if valid {
		t.Fatal("expected valid = false for a nonexistent ticket file")
	}
	if summary != "no valid ticket" {
		t.Fatalf("summary = %q, want %q", summary, "no valid ticket")
	}
	if detail == "" {
		t.Fatal("expected a non-empty detail message")
	}
}

func TestPidRunsLastExitPatterns(t *testing.T) {
	t.Parallel()

	sample := `gui/501/com.example.daemon = {
	active count = 1
	state = running

	pid = 12345
	runs = 7
	last exit code = 1
}
`
	if m := pidPattern.FindStringSubmatch(sample); m == nil || m[1] != "12345" {
		t.Fatalf("pidPattern match = %#v, want pid 12345", m)
	}
	if m := runsPattern.FindStringSubmatch(sample); m == nil || m[1] != "7" {
		t.Fatalf("runsPattern match = %#v, want runs 7", m)
	}
	if m := lastExitPattern.FindStringSubmatch(sample); m == nil || m[1] != "1" {
		t.Fatalf("lastExitPattern match = %#v, want last exit code 1", m)
	}
}

func TestDaemonsConfigWarningsCatchesTypos(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	body := `
daemons:
  kerberoskeepalive:
    lable: kerberoskeepalive
  omt:
    label: com.schretzi.omt
`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("os.WriteFile() error = %v", err)
	}
	warnings, err := daemonsConfigWarnings(path)
	if err != nil {
		t.Fatalf("daemonsConfigWarnings() error = %v", err)
	}
	joined := strings.Join(warnings, "\n")
	if !strings.Contains(joined, "daemons.kerberoskeepalive is not a recognized daemon") {
		t.Fatalf("expected a warning about the unrecognized daemon key, got: %v", warnings)
	}
	if !strings.Contains(joined, "kerberos_keep_alive") {
		t.Fatalf("expected the warning to mention the correct key kerberos_keep_alive, got: %v", warnings)
	}
}

func TestDaemonsConfigWarningsCleanConfig(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	body := `
daemons:
  unbound:
    label: net.unbound
    scope: system
  kerberos_keep_alive:
    label: com.example.kerberoskeepalive
  omt:
    label: com.schretzi.omt
  vpn:
    label: com.schretzi.corp-vpn
    interface: utun99
  tunneling:
    label: com.schretzi.tunneling
`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("os.WriteFile() error = %v", err)
	}
	warnings, err := daemonsConfigWarnings(path)
	if err != nil {
		t.Fatalf("daemonsConfigWarnings() error = %v", err)
	}
	if len(warnings) != 0 {
		t.Fatalf("expected no warnings for a valid daemons: block, got: %v", warnings)
	}
}

// The warning used to hand-list the valid keys and had drifted out of step
// with knownDaemonKeys, telling people vpn was invalid when it was not.
func TestDaemonsConfigWarningListsEveryKnownKey(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte("daemons:\n  nosuchdaemon:\n    label: x\n"), 0o600); err != nil {
		t.Fatalf("os.WriteFile() error = %v", err)
	}
	warnings, err := daemonsConfigWarnings(path)
	if err != nil {
		t.Fatalf("daemonsConfigWarnings() error = %v", err)
	}
	joined := strings.Join(warnings, "\n")
	for key := range knownDaemonKeys {
		if !strings.Contains(joined, key) {
			t.Errorf("warning does not mention the valid key %q: %v", key, warnings)
		}
	}
}

// Every daemon key that config accepts must also produce a row in observe -
// otherwise it validates cleanly and then silently does nothing, which is
// what daemons.tunneling did before it was wired up.
func TestObserveHasARowForEveryKnownDaemonKey(t *testing.T) {
	t.Parallel()

	rows := newObserveModel(Config{}).rows
	haveRow := make(map[string]bool, len(rows))
	for _, r := range rows {
		haveRow[r.configKey] = true
	}
	for key := range knownDaemonKeys {
		if !haveRow[key] {
			t.Errorf("daemons.%s is a recognized config key but observe has no row for it", key)
		}
	}
}

func TestDaemonsConfigWarningsNoDaemonsBlock(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte("local_proxy:\n  host: 127.0.0.1\n"), 0o600); err != nil {
		t.Fatalf("os.WriteFile() error = %v", err)
	}
	warnings, err := daemonsConfigWarnings(path)
	if err != nil {
		t.Fatalf("daemonsConfigWarnings() error = %v", err)
	}
	if len(warnings) != 0 {
		t.Fatalf("expected no warnings when daemons: is absent, got: %v", warnings)
	}
}

func TestVPNInterfaceStatusMissingInterface(t *testing.T) {
	t.Parallel()

	up, detail := vpnInterfaceStatus("utun999999-does-not-exist")
	if up {
		t.Fatal("expected up = false for a nonexistent interface")
	}
	if detail == "" {
		t.Fatal("expected a non-empty detail message")
	}
}

func TestVPNInetPattern(t *testing.T) {
	t.Parallel()

	sample := `utun99: flags=8051<UP,POINTOPOINT,RUNNING,MULTICAST> mtu 1400
	inet 10.1.2.3 --> 10.1.2.3 netmask 0xffffff00
`
	m := vpnInetPattern.FindStringSubmatch(sample)
	if m == nil || m[1] != "10.1.2.3" {
		t.Fatalf("vpnInetPattern match = %#v, want inet 10.1.2.3", m)
	}
}

func TestParseTunnelingStatus(t *testing.T) {
	t.Parallel()

	const header = "NAME  KIND  LOCAL            STATE  DESTINATION      VIA\n"

	tests := []struct {
		name        string
		out         string
		wantSummary string
		wantLines   []string
	}{
		{
			name:        "empty output",
			out:         "",
			wantSummary: "no tunnels configured",
		},
		{
			name:        "header only",
			out:         strings.TrimRight(header, "\n"),
			wantSummary: "no tunnels configured",
		},
		{
			name: "all open lists nothing",
			out: header +
				"jump-dev  gcp  127.0.0.1:10022  OPEN  host:22   iap p/z\n" +
				"k8s-dev   ssh  127.0.0.1:10443  OPEN  10.0.0.1:443  me@localhost:10022",
			wantSummary: "2/2 tunnels open",
		},
		{
			name: "closed tunnels are named",
			out: header +
				"jump-dev  gcp  127.0.0.1:10022  OPEN    host:22       iap p/z\n" +
				"k8s-dev   ssh  127.0.0.1:10443  CLOSED  10.0.0.1:443  me@localhost:10022",
			wantSummary: "1/2 tunnels open",
			wantLines:   []string{"closed: k8s-dev"},
		},
		{
			// A tunnel named "OPEN..." must not be miscounted, and the
			// STATE column is matched as a whole field, not a substring.
			name: "state is matched as a whole field",
			out: header +
				"reopened  ssh  127.0.0.1:1  CLOSED  h:1  v",
			wantSummary: "0/1 tunnels open",
			wantLines:   []string{"closed: reopened"},
		},
		{
			name: "long closed list is capped",
			out: header +
				"a  ssh  127.0.0.1:1  CLOSED  h:1  v\n" +
				"b  ssh  127.0.0.1:2  CLOSED  h:1  v\n" +
				"c  ssh  127.0.0.1:3  CLOSED  h:1  v\n" +
				"d  ssh  127.0.0.1:4  CLOSED  h:1  v\n" +
				"e  ssh  127.0.0.1:5  CLOSED  h:1  v\n" +
				"f  ssh  127.0.0.1:6  CLOSED  h:1  v\n" +
				"g  ssh  127.0.0.1:7  CLOSED  h:1  v\n" +
				"h  ssh  127.0.0.1:8  CLOSED  h:1  v",
			wantSummary: "0/8 tunnels open",
			wantLines:   []string{"closed: a, b, c, d, e, f (+2 more)"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			summary, lines := parseTunnelingStatus(tt.out)
			if summary != tt.wantSummary {
				t.Errorf("summary = %q, want %q", summary, tt.wantSummary)
			}
			if !slices.Equal(lines, tt.wantLines) {
				t.Errorf("lines = %v, want %v", lines, tt.wantLines)
			}
		})
	}
}
