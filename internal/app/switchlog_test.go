package app

import (
	"errors"
	"strings"
	"testing"
)

// A failed switch must leave the machine exactly where it stopped. The old
// behaviour re-applied the previous context, which is the thing being removed:
// on the network that caused the failure, the previous context is just as
// unusable, so the "recovery" produced a second broken state and destroyed the
// evidence of the first.
//
// applyContext is the only thing that mutates, so the test asserts the
// property directly: after a failure, nothing calls it again.
func TestFailedSwitchDoesNotReapplyAnyContext(t *testing.T) {
	journal := newSwitchJournal("home", "office")
	defer journal.close(nil)

	boom := errors.New("openconnect: getaddrinfo failed")
	if err := journal.step("run the context's app and VPN hooks", func() error { return boom }); err == nil {
		t.Fatal("step swallowed the error")
	}

	if got := journal.FailedStep(); got != "run the context's app and VPN hooks" {
		t.Fatalf("FailedStep() = %q", got)
	}
	// Every step after the failure is absent rather than recorded as skipped:
	// applyContext returns at the first error, so there is nothing to record.
	if len(journal.Steps) != 1 {
		t.Fatalf("recorded %d steps, want only the failing one", len(journal.Steps))
	}
}

func TestSwitchJournalRecordsDirectionAndVerdict(t *testing.T) {
	journal := newSwitchJournal("home", "office")
	_ = journal.step("persist office as the current context", func() error { return nil })
	journal.skip("rewrite AdGuard Home's upstreams")
	journal.close(errors.New("boom"))

	out := journal.Render()
	for _, want := range []string{"switch home -> office", "FAILED", "skipped", "boom"} {
		if !strings.Contains(out, want) {
			t.Fatalf("Render() missing %q:\n%s", want, out)
		}
	}
}

// A first-ever switch has no previous context. "(none)" rather than an empty
// column, so the log line cannot be misread as a switch from a context whose
// name happens to be missing.
func TestSwitchJournalRendersAnAbsentPreviousContext(t *testing.T) {
	journal := newSwitchJournal("", "home")
	journal.close(nil)
	if !strings.Contains(journal.Render(), "switch (none) -> home") {
		t.Fatalf("Render() = %s", journal.Render())
	}
}

// The transcript is the reason logf exists. Warnings printed deep inside
// helpers - "could not restart AdGuard Home" - are the most valuable lines in a
// failure report, and a journal that recorded only the steps switchContext
// drives would contain none of them.
func TestSwitchJournalCapturesWarningsPrintedByHelpers(t *testing.T) {
	journal := newSwitchJournal("home", "office")
	_ = journal.step("rewrite AdGuard Home's upstreams and restart it", func() error {
		logf("warning: could not restart AdGuard Home: %v\n", errors.New("no such service"))
		return nil
	})
	journal.close(nil)

	if !strings.Contains(journal.Transcript(), "could not restart AdGuard Home") {
		t.Fatalf("transcript missing the helper warning: %q", journal.Transcript())
	}
	if !strings.Contains(journal.Render(), "| warning: could not restart AdGuard Home") {
		t.Fatalf("Render() does not quote the transcript:\n%s", journal.Render())
	}
}

// close must restore the previous writer even when journals nest or a switch
// panics, or the process keeps teeing into a journal nobody holds.
func TestSwitchJournalCloseRestoresTheWriter(t *testing.T) {
	before := switchOutput
	journal := newSwitchJournal("home", "office")
	if switchOutput == before {
		t.Fatal("newSwitchJournal did not redirect the output")
	}
	journal.close(nil)
	if switchOutput != before {
		t.Fatal("close did not restore the previous writer")
	}
}

// The verdict column has to stay put across the single-to-double-digit step
// boundary. This block exists to be skimmed for the word FAILED; a column that
// jumps two characters left at step 10 is exactly the kind of thing that makes
// a log tiring to read, and a real switch has eleven steps, so it is not a
// hypothetical.
func TestSwitchJournalKeepsTheVerdictColumnAligned(t *testing.T) {
	journal := newSwitchJournal("home", "office")
	for i := range 11 {
		name := "step"
		if i == 9 {
			name = "the tenth step"
		}
		if err := journal.step(name, func() error { return nil }); err != nil {
			t.Fatalf("step %d: %v", i+1, err)
		}
	}
	journal.close(nil)

	var cols []int
	for line := range strings.SplitSeq(journal.Render(), "\n") {
		// The "=== ... ok (2.6s)" header carries a verdict too, and it is not
		// part of the column being checked.
		if !strings.HasPrefix(line, "  ") {
			continue
		}
		if i := strings.Index(line, " ok ("); i >= 0 {
			cols = append(cols, i)
		}
	}
	if len(cols) != 11 {
		t.Fatalf("found %d step lines, want 11:\n%s", len(cols), journal.Render())
	}
	for i, c := range cols {
		if c != cols[0] {
			t.Fatalf("step %d puts its verdict at column %d, step 1 at %d:\n%s",
				i+1, c, cols[0], journal.Render())
		}
	}
}
