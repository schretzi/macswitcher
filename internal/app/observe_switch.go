package app

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sort"
	"strings"

	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
)

// switchPhase is where the modal is in the pick-then-run sequence. The two
// phases have different keymaps and different bodies, and the running phase
// deliberately refuses to close: see handleSwitchKey.
type switchPhase int

const (
	// switchPhasePick is the context list. esc cancels, enter starts.
	switchPhasePick switchPhase = iota
	// switchPhaseRun is the live output of a switch still in flight.
	switchPhaseRun
	// switchPhaseDone is the same output after the switch finished, now
	// closable with enter or esc.
	switchPhaseDone
)

// switchModal is the `S` modal: it picks a context, runs the switch, and
// shows its output. It mirrors logModal's shape (an open flag plus a
// viewport) so both modals are driven the same way from Update and View.
type switchModal struct {
	open     bool
	phase    switchPhase
	contexts []string
	// current is the context that was active when the modal opened, kept so
	// the list can mark it even after the cursor moves off it.
	current  string
	cursor   int
	selected string
	viewport viewport.Model
	lines    []string
	err      error
	// events carries the running switch's output; a nil channel means no
	// switch is in flight, which is how a late event is recognised as stale.
	events chan switchEvent
	// seq invalidates the events of a superseded run, so output from a
	// switch whose modal was closed cannot land in a later one.
	seq int
}

// switchEvent is one line of a running switch, or its completion. A single
// channel carries both so that "finished" cannot overtake the last lines of
// output the way two channels would allow.
type switchEvent struct {
	line string
	done bool
	err  error
}

// switchEventMsg delivers one switchEvent to Update. ok is false once the
// channel is closed, which only happens after the done event.
type switchEventMsg struct {
	seq   int
	event switchEvent
	ok    bool
}

// switchContexts lists the configured context names in a stable order. The
// config holds them in a map, so without the sort the modal would reshuffle
// itself on every open.
func switchContexts(cfg Config) []string {
	names := make([]string, 0, len(cfg.Contexts))
	for name := range cfg.Contexts {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// openSwitchModal opens the context picker with the cursor on the active
// context, so the common case (glance at where you are, move one row, enter)
// needs no hunting.
func (m observeModel) openSwitchModal() (tea.Model, tea.Cmd) {
	names := switchContexts(m.cfg)
	if len(names) == 0 {
		m.message = "no contexts configured"
		m.messageIsErr = true
		return m, nil
	}
	width, height := m.modalViewportSize()
	m.switcher = switchModal{
		open:     true,
		phase:    switchPhasePick,
		contexts: names,
		current:  m.cfg.CurrentContext,
		cursor:   indexOfContext(names, m.cfg.CurrentContext),
		viewport: newLogViewport(width, height),
		seq:      m.switcher.seq,
	}
	return m, nil
}

// indexOfContext returns the position of name, or 0 when the active context
// is unset or no longer configured - the cursor has to start somewhere, and
// the first row is a harmless default because nothing runs without enter.
func indexOfContext(names []string, name string) int {
	for i, candidate := range names {
		if candidate == name {
			return i
		}
	}
	return 0
}

func (m observeModel) handleSwitchKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch m.switcher.phase {
	case switchPhasePick:
		return m.handleSwitchPickKey(msg)
	case switchPhaseRun:
		// A context switch rewrites DNS, the proxy and several daemons in
		// sequence. Closing the modal would not stop it, and interrupting it
		// halfway would leave the machine in a state no rollback covers, so
		// the running phase swallows every key.
		return m, nil
	case switchPhaseDone:
		switch msg.String() {
		case "enter", keyEscape, "q", keyInterrupt:
			return m.closeSwitchModal()
		}
		var cmd tea.Cmd
		m.switcher.viewport, cmd = m.switcher.viewport.Update(msg)
		return m, cmd
	}
	return m, nil
}

func (m observeModel) handleSwitchPickKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case keyEscape, "q", keyInterrupt, "S":
		return m.closeSwitchModal()
	case "up", "k":
		if m.switcher.cursor > 0 {
			m.switcher.cursor--
		}
		return m, nil
	case "down", "j":
		if m.switcher.cursor < len(m.switcher.contexts)-1 {
			m.switcher.cursor++
		}
		return m, nil
	case "enter":
		return m.startSwitch()
	}
	return m, nil
}

// closeSwitchModal drops the modal and, with it, any in-flight run's claim on
// the model: bumping seq makes every event still queued from that run stale.
func (m observeModel) closeSwitchModal() (tea.Model, tea.Cmd) {
	if m.switcher.phase == switchPhaseDone && m.switcher.err == nil && m.switcher.selected != "" {
		// The switch rewrote config.yaml's current_context. Reload so the
		// list behind the modal, and the next open of this one, agree with
		// what is actually active.
		if cfg, err := loadConfig(m.cfgPath); err == nil {
			m.cfg = cfg
		}
	}
	m.switcher = switchModal{seq: m.switcher.seq + 1}
	return m, nil
}

func (m observeModel) startSwitch() (tea.Model, tea.Cmd) {
	selected := m.switcher.contexts[m.switcher.cursor]
	m.switcher.selected = selected
	m.switcher.phase = switchPhaseRun
	m.switcher.lines = nil
	m.switcher.err = nil
	m.switcher.seq++

	events := make(chan switchEvent, switchEventBuffer)
	m.switcher.events = events
	m.switcher.setContent([]string{styleDim.Render("switching to " + selected + "…")})
	return m, tea.Batch(
		runSwitchCmd(m.cfgPath, selected, events),
		waitSwitchEventCmd(m.switcher.seq, events),
	)
}

// switchEventBuffer lets the switch keep making progress while the UI is
// between frames. A switch prints on the order of dozens of lines, so this
// holds all of them and the producer never blocks on the renderer.
const switchEventBuffer = 128

// waitSwitchEventCmd takes the next event off the channel. Update re-issues
// it after every event, which is bubbletea's way of consuming a stream
// without holding a reference to the running Program.
func waitSwitchEventCmd(seq int, events chan switchEvent) tea.Cmd {
	return func() tea.Msg {
		event, ok := <-events
		return switchEventMsg{seq: seq, event: event, ok: ok}
	}
}

// runSwitchCmd runs the switch as `macswitcher switch <context>` in a child
// process and streams its output into events.
//
// The switch path prints progress with fmt.Printf from a couple of dozen
// call sites spread over context.go, network.go, system.go and adguard.go.
// Threading an io.Writer through all of them would be a wide refactor whose
// every missed call site is a line that silently vanishes from this modal,
// and swapping os.Stdout for a pipe would mutate a global that bubbletea's
// renderer and every other goroutine share. Re-invoking our own binary
// sidesteps both: the child owns its stdout, the TUI's is untouched, and the
// modal shows byte for byte what `macswitcher switch` prints on a terminal.
//
// Nothing in the switch path reads stdin (the only interactive prompts are in
// `proxy run` and `proxy password-set`, and sudo is only ever called with
// -n), so the child needs no terminal and can run detached from ours.
func runSwitchCmd(cfgPath, selected string, events chan switchEvent) tea.Cmd {
	return func() tea.Msg {
		defer close(events)

		self, err := os.Executable()
		if err != nil {
			events <- switchEvent{done: true, err: fmt.Errorf("locating macswitcher binary: %w", err)}
			return nil
		}
		// Not filepath.EvalSymlinks: on a Homebrew install that resolves
		// /opt/homebrew/bin/macswitcher to the versioned Caskroom path.
		//nolint:noctx // a context switch reconfigures DNS, the proxy and several daemons in sequence; killing it on a deadline would leave the machine half-switched, which is worse than a slow switch
		cmd := exec.Command(self, "--config", cfgPath, "switch", selected) // #nosec G204 -- self is our own binary and selected is a context name that came from the loaded config, not from input
		cmd.Stdin = nil

		pipe, err := cmd.StdoutPipe()
		if err != nil {
			events <- switchEvent{done: true, err: err}
			return nil
		}
		cmd.Stderr = cmd.Stdout

		if err := cmd.Start(); err != nil {
			events <- switchEvent{done: true, err: err}
			return nil
		}
		streamSwitchOutput(pipe, events)
		events <- switchEvent{done: true, err: cmd.Wait()}
		return nil
	}
}

// streamSwitchOutput forwards each line of the child's output. Lines longer
// than bufio's default token limit are split rather than dropped, because a
// switch that fails on a long command line must still be readable.
func streamSwitchOutput(r io.Reader, events chan<- switchEvent) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		events <- switchEvent{line: strings.TrimRight(scanner.Text(), "\r")}
	}
	if err := scanner.Err(); err != nil && !errors.Is(err, os.ErrClosed) {
		events <- switchEvent{line: "reading switch output: " + err.Error()}
	}
}

// handleSwitchEvent appends one line, or finishes the run. Events from a
// superseded run - a modal closed mid-switch, or a second switch started
// after the first - carry an old seq and are dropped.
func (m observeModel) handleSwitchEvent(msg switchEventMsg) (tea.Model, tea.Cmd) {
	if !m.switcher.open || msg.seq != m.switcher.seq {
		return m, nil
	}
	if !msg.ok {
		// The channel closed without a done event, which only happens if the
		// producer died. Treat it as the end of the run rather than waiting
		// on a channel that will never deliver again.
		if m.switcher.phase == switchPhaseRun {
			m.switcher.phase = switchPhaseDone
			m.switcher.events = nil
		}
		return m, nil
	}
	if msg.event.done {
		m.switcher.phase = switchPhaseDone
		m.switcher.events = nil
		m.switcher.err = msg.event.err
		if msg.event.err != nil {
			m.switcher.appendLine(styleError.Render("switch failed: " + msg.event.err.Error()))
		} else {
			m.switcher.appendLine(styleRunning.Render("switch to " + m.switcher.selected + " finished"))
		}
		return m, nil
	}
	m.switcher.appendLine(msg.event.line)
	return m, waitSwitchEventCmd(msg.seq, m.switcher.events)
}

// appendLine adds a line and keeps the viewport pinned to the newest output,
// which is what someone watching a switch run wants; the done phase leaves
// scrolling to the reader.
func (s *switchModal) appendLine(line string) {
	s.lines = append(s.lines, line)
	s.viewport.SetContent(strings.Join(s.lines, "\n"))
	s.viewport.GotoBottom()
}

func (s *switchModal) setContent(lines []string) {
	s.lines = lines
	s.viewport.SetContent(strings.Join(lines, "\n"))
	s.viewport.GotoBottom()
}

func (s *switchModal) setSize(width, height int) {
	s.viewport.Width = width
	s.viewport.Height = height
}
