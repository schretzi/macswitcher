package app

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// A switch is the one operation on this machine that can take the network
// away, and until now it left no trace. It printed to a terminal, and when the
// switch failed the operator was left with whatever had scrolled past - on a
// machine that could no longer reach anything to ask for help with.
//
// So every switch records a transcript: which context to which, every step in
// order with its timing, every warning, and the step it died on. The transcript
// is appended to a file that survives the terminal, and a failed switch drops a
// full snapshot next to it.
//
// This is what replaces the rollback. Undoing a half-finished switch only
// looked like a safety net: coming back to the previous context restores the
// setup for a network the machine is no longer on, so the office failure landed
// the operator back on a home configuration that was equally dead - and now
// with the evidence gone too. Reporting the exact state beats guessing at a
// state to return to.

// switchOutput is where the switch path's own output goes. It is a var so a
// switch can tee itself into a transcript, and so tests can read it.
//
// Every print in the switch path goes through logf rather than fmt.Printf.
// That is what makes the transcript faithful: the warnings that matter most -
// "could not restart AdGuard Home", "could not set the local proxy" - are
// printed deep inside helpers, and a transcript that recorded only the steps
// switchContext knows about would be missing exactly them.
var switchOutput io.Writer = os.Stdout

func logf(format string, a ...any) {
	fmt.Fprintf(switchOutput, format, a...)
}

// journalStep is one step of a switch, in the order applyContext runs them.
type journalStep struct {
	Name     string
	Started  time.Time
	Duration time.Duration
	Err      error
	// Skipped marks a step the context did not ask for (no upstreams, no
	// network location). Recorded rather than omitted: "AdGuard upstreams
	// were not touched" is a fact worth having when the intranet is gone,
	// and an absent line reads the same as a step that never ran.
	Skipped bool
}

// switchJournal records one switch attempt.
type switchJournal struct {
	From       string
	To         string
	Started    time.Time
	Finished   time.Time
	Steps      []journalStep
	Err        error
	transcript strings.Builder
	prevOut    io.Writer
	mu         sync.Mutex
}

func newSwitchJournal(from, to string) *switchJournal {
	j := &switchJournal{From: from, To: to, Started: time.Now()}
	j.prevOut = switchOutput
	switchOutput = io.MultiWriter(j.prevOut, &j.transcript)
	return j
}

// close restores the output writer. Always deferred, so a panic in the switch
// path cannot leave the process teeing into a dead journal.
func (j *switchJournal) close(err error) {
	if j == nil {
		return
	}
	j.Finished = time.Now()
	j.Err = err
	switchOutput = j.prevOut
}

// step runs one named step and records how it went.
//
// The name is what the operator reads in the report, so it says what the step
// does to the machine ("point the resolvers at the local resolver"), not which
// function was called.
func (j *switchJournal) step(name string, fn func() error) error {
	if fn == nil {
		j.record(journalStep{Name: name, Started: time.Now(), Skipped: true})
		return nil
	}
	started := time.Now()
	err := fn()
	j.record(journalStep{Name: name, Started: started, Duration: time.Since(started), Err: err})
	return err
}

// skip records a step the context did not ask for.
func (j *switchJournal) skip(name string) {
	j.record(journalStep{Name: name, Started: time.Now(), Skipped: true})
}

func (j *switchJournal) record(s journalStep) {
	if j == nil {
		return
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	j.Steps = append(j.Steps, s)
}

// FailedStep names the step the switch died on, or "" when it did not.
func (j *switchJournal) FailedStep() string {
	for _, s := range j.Steps {
		if s.Err != nil {
			return s.Name
		}
	}
	return ""
}

// Transcript is everything the switch printed, warnings included.
func (j *switchJournal) Transcript() string { return j.transcript.String() }

// Render writes the journal as the switch log entry.
//
// One self-contained block per switch, headed by a line that carries the whole
// answer to "what happened": time, direction, verdict. Someone scanning the
// file with grep should not have to read a block to decide whether it is the
// one they want.
func (j *switchJournal) Render() string {
	var b strings.Builder
	verdict := "ok"
	if j.Err != nil {
		verdict = "FAILED"
	}
	from := j.From
	if strings.TrimSpace(from) == "" {
		from = "(none)"
	}
	fmt.Fprintf(&b, "=== %s  switch %s -> %s  %s (%s)\n",
		j.Started.Format(time.RFC3339), from, j.To, verdict, j.Finished.Sub(j.Started).Round(time.Millisecond))

	// The step number is right-aligned to the width of the largest one, so
	// the verdict column does not jump two characters left at step 10. The
	// whole point of this block is to be skimmed for the word FAILED, and a
	// column that moves halfway down defeats that.
	numWidth := len(strconv.Itoa(len(j.Steps)))
	for i, s := range j.Steps {
		label := fmt.Sprintf("%*d. %s", numWidth, i+1, s.Name)
		indent := strings.Repeat(" ", numWidth+4)
		switch {
		case s.Skipped:
			fmt.Fprintf(&b, "  %-54s skipped\n", label)
		case s.Err != nil:
			fmt.Fprintf(&b, "  %-54s FAILED after %s\n%s",
				label, s.Duration.Round(time.Millisecond), indentLines(s.Err.Error(), indent))
		default:
			fmt.Fprintf(&b, "  %-54s ok (%s)\n", label, s.Duration.Round(time.Millisecond))
		}
	}
	// Only report the journal's error separately when it is not already
	// spelled out by the failing step. A preflight error runs to four lines
	// of diagnosis; printing it twice in a block this size buries the step
	// list it belongs to.
	if j.Err != nil && j.Err.Error() != j.failedStepError() {
		fmt.Fprintf(&b, "  error: %s", indentLines(j.Err.Error(), "  "))
	}
	if t := strings.TrimSpace(j.Transcript()); t != "" {
		b.WriteString("  --- output ---\n")
		for line := range strings.SplitSeq(t, "\n") {
			fmt.Fprintf(&b, "  | %s\n", line)
		}
	}
	b.WriteString("\n")
	return b.String()
}

// indentLines prefixes every line, not just the first. Errors here are
// routinely multi-line - preflight's diagnosis is four lines - and a
// continuation starting at column zero reads as a new record, because the
// blocks in this file are delimited by a line starting with "===".
func indentLines(text, indent string) string {
	var b strings.Builder
	for line := range strings.SplitSeq(strings.TrimRight(text, "\n"), "\n") {
		b.WriteString(indent)
		b.WriteString(line)
		b.WriteString("\n")
	}
	return b.String()
}

// failedStepError is the error text of the step that failed, or "" if none
// did. Used only to avoid printing the same diagnosis twice.
func (j *switchJournal) failedStepError() string {
	for _, s := range j.Steps {
		if s.Err != nil {
			return s.Err.Error()
		}
	}
	return ""
}

// switchLogPath is the running record of every switch, successful or not.
//
// ~/Library/Logs/<name>.log, per the conventions the other jobs follow, and
// safe to rotate with newsyslog without the rename trap: `macswitcher switch`
// is a short-lived process that opens the file, appends and exits, so it never
// holds an fd across a rotation.
func switchLogPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "Library", "Logs", "macswitcher-switch.log"), nil
}

// appendSwitchLog records the journal. Failing to write it must never fail the
// switch: this is diagnostics, and losing them is not a reason to refuse to
// change the network.
func appendSwitchLog(j *switchJournal) string {
	path, err := switchLogPath()
	if err != nil {
		return ""
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return ""
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600) // #nosec G304 -- path is derived from the user's home, not from input
	if err != nil {
		return ""
	}
	defer func() { _ = f.Close() }()
	if _, err := f.WriteString(j.Render()); err != nil {
		return ""
	}
	return path
}
