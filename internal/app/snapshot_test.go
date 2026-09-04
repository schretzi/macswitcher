package app

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSnapshotReportLeadsWithTheTransitionAndTheFailure(t *testing.T) {
	s := &snapshot{
		takenAt:    time.Date(2026, 9, 4, 14, 30, 0, 0, time.UTC),
		reason:     "switch home -> office failed",
		from:       "home",
		to:         "office",
		failedStep: "verify DNS resolves",
		switchErr:  "puma.corp.example did not resolve",
		steps: []journalStep{
			{Name: "persist office as the current context", Duration: time.Millisecond},
			{Name: "rewrite AdGuard Home's upstreams", Skipped: true},
			{Name: "verify DNS resolves", Err: errors.New("nope")},
		},
	}
	got := s.report()

	for _, want := range []string{
		"`home` -> `office`",
		"failed at**: verify DNS resolves",
		"puma.corp.example did not resolve",
		"skipped (not configured for this context)",
		"**FAILED**",
		"were **not** undone",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("report() missing %q:\n%s", want, got)
		}
	}
}

// A snapshot taken by hand has no switch to describe, and must not pretend
// otherwise - "-> " with an empty target would read as a switch to nowhere.
func TestSnapshotReportWithoutASwitchShowsTheCurrentContext(t *testing.T) {
	s := &snapshot{takenAt: time.Now(), from: "home", reason: "taken by hand"}
	got := s.report()
	if !strings.Contains(got, "current context**: `home`") {
		t.Fatalf("report() = %s", got)
	}
	if strings.Contains(got, "->") {
		t.Fatalf("report() invented a transition:\n%s", got)
	}
}

// The state word has to name the OUTERMOST reason, because that is the one
// worth acting on: reporting "not running" for a daemon whose plist is missing
// sends the reader off to check a process that was never going to exist.
func TestDaemonStateWordNamesTheOutermostCause(t *testing.T) {
	tests := []struct {
		name   string
		status daemonStatus
		want   string
	}{
		{"unconfigured", daemonStatus{Err: errors.New("not configured")}, "not configured"},
		{"no plist", daemonStatus{}, "not installed"},
		{"installed only", daemonStatus{Installed: true}, "not loaded"},
		{"loaded but dead", daemonStatus{Installed: true, Loaded: true}, "loaded, not running"},
		{"healthy", daemonStatus{Installed: true, Loaded: true, Running: true, PID: 42}, "running"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := daemonStateWord(tt.status); got != tt.want {
				t.Fatalf("daemonStateWord() = %q, want %q", got, tt.want)
			}
		})
	}
}

// A snapshot is made to be sent to somebody. Credentials inline in a proxy URL
// are the one thing in reach of these collectors that must not travel with it.
func TestRedactStripsInlineCredentials(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{"http://alice:s3cr3t@proxy.corp.example:8080", "http://REDACTED:REDACTED@proxy.corp.example:8080"},
		{"https://u:p@host/path", "https://REDACTED:REDACTED@host/path"},
		// Left alone: no credentials, and a snapshot that mangled ordinary
		// URLs would be harder to read for no gain.
		{"http://proxy.corp.example:8080", "http://proxy.corp.example:8080"},
		{"upstream 10.0.0.1:53", "upstream 10.0.0.1:53"},
	}
	for _, tt := range tests {
		if got := redact(tt.in); got != tt.want {
			t.Fatalf("redact(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestRedactAppliesToTheRenderedReport(t *testing.T) {
	s := &snapshot{
		takenAt: time.Now(),
		reason:  "proxy http://alice:s3cr3t@proxy.corp.example:8080 refused",
	}
	if strings.Contains(s.report(), "s3cr3t") {
		t.Fatal("report() leaked a credential")
	}
}

// The directory name is what pruning sorts on, so lexical order has to equal
// chronological order. A format with a variable-width component would break
// that silently, and only once a snapshot was actually needed.
func TestSnapshotDirNamesSortChronologically(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	early, err := snapshotDir(time.Date(2026, 9, 4, 9, 5, 1, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	late, err := snapshotDir(time.Date(2026, 9, 4, 14, 5, 1, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(early) >= filepath.Base(late) {
		t.Fatalf("%q does not sort before %q", filepath.Base(early), filepath.Base(late))
	}
}

// Nothing else prunes these: newsyslog rotates files, not directories, so the
// command that creates them is the only thing positioned to clean up.
func TestPruneSnapshotsKeepsTheNewest(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	parent := filepath.Join(home, "Library", "Logs", "macswitcher-snapshots")
	names := []string{
		"2026-09-01T10-00-00",
		"2026-09-02T10-00-00",
		"2026-09-03T10-00-00",
		"2026-09-04T10-00-00",
	}
	for _, n := range names {
		if err := os.MkdirAll(filepath.Join(parent, n), 0o750); err != nil {
			t.Fatal(err)
		}
	}
	pruneSnapshots(2)

	entries, err := os.ReadDir(parent)
	if err != nil {
		t.Fatal(err)
	}
	var left []string
	for _, e := range entries {
		left = append(left, e.Name())
	}
	if len(left) != 2 || left[0] != names[2] || left[1] != names[3] {
		t.Fatalf("kept %v, want the two newest", left)
	}
}

// Pruning must survive the parent not existing yet - the first snapshot on a
// machine creates it, and a panic there would take the snapshot with it.
func TestPruneSnapshotsToleratesAMissingDirectory(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	pruneSnapshots(5)
}

// A manual snapshot has no journal, so the switch log on disk is the only
// record of what went wrong. Cutting it by bytes would slice through the
// middle of an entry; entries are cut on their header instead.
func TestLastSwitchLogEntriesCutsOnWholeEntries(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	path, err := switchLogPath()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatal(err)
	}
	content := "=== one\n  1. step ok\n=== two\n  1. step ok\n=== three\n  1. step ok\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	got := lastSwitchLogEntries(2)
	if strings.Contains(got, "=== one") {
		t.Fatalf("kept more entries than asked:\n%s", got)
	}
	for _, want := range []string{"=== two", "=== three"} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q:\n%s", want, got)
		}
	}
}

func TestAppendSwitchLogWritesOneEntryPerSwitch(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	for _, to := range []string{"office", "home"} {
		j := newSwitchJournal("home", to)
		_ = j.step("persist "+to+" as the current context", func() error { return nil })
		j.close(nil)
		if got := appendSwitchLog(j); got == "" {
			t.Fatal("appendSwitchLog reported no path")
		}
	}

	path, err := switchLogPath()
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(string(b), "=== "); got != 2 {
		t.Fatalf("switch log holds %d entries, want 2:\n%s", got, b)
	}
}

// The automatic snapshot must carry the history as well as the live journal.
// "Has this failed before, and how often" decides how the rest of the snapshot
// is read, and the history is the only thing that answers it. The failing
// switch itself is NOT in there - its block is appended after the snapshot is
// written - so the two files complement each other.
func TestSnapshotWithAJournalStillCarriesTheHistory(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	// collectProbes resolves through dnsServiceOps, which is armed to panic
	// so no test can quietly depend on the developer's own network.
	stubDNSServiceOps(t, &fakeDNS{configured: map[string][]string{}})

	logPath := filepath.Join(home, "Library", "Logs", "macswitcher-switch.log")
	if err := os.MkdirAll(filepath.Dir(logPath), 0o750); err != nil {
		t.Fatal(err)
	}
	prior := "=== 2026-09-01T10:00:00+02:00  switch home -> office  FAILED (1s)\n  1. preflight  FAILED after 1s\n\n"
	if err := os.WriteFile(logPath, []byte(prior), 0o600); err != nil {
		t.Fatal(err)
	}

	journal := newSwitchJournal("home", "office")
	_ = journal.step("preflight", func() error { return errors.New("boom") })
	journal.close(errors.New("boom"))

	dir, err := writeSnapshot("", Config{CurrentContext: "home"}, snapshotRequest{
		Journal: journal,
		Reason:  "test",
	})
	if err != nil {
		t.Fatalf("writeSnapshot() = %v", err)
	}

	for _, name := range []string{"switch.log", "switch-history.log"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Fatalf("%s missing from an automatic snapshot: %v", name, err)
		}
	}
	history, err := os.ReadFile(filepath.Join(dir, "switch-history.log"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(history), "2026-09-01T10:00:00") {
		t.Fatalf("history lost the earlier switch:\n%s", history)
	}
}
