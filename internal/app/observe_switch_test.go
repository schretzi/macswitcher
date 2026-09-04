package app

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

func switchTestConfig() Config {
	return Config{
		CurrentContext: "office",
		Contexts: map[string]SwitchContext{
			"remote": {},
			"office": {},
			"home":   {},
		},
	}
}

func TestSwitchContextsAreSorted(t *testing.T) {
	got := switchContexts(switchTestConfig())
	want := []string{"home", "office", "remote"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}

// The cursor starting anywhere but the active context would make the common
// case - see where you are, step one row, enter - a hunt through the list.
func TestOpenSwitchModalStartsOnCurrentContext(t *testing.T) {
	model, _ := newObserveModel(switchTestConfig(), "").openSwitchModal()
	m := asObserve(t, model)

	if !m.switcher.open {
		t.Fatal("modal not open")
	}
	if m.switcher.phase != switchPhasePick {
		t.Fatalf("phase = %v, want pick", m.switcher.phase)
	}
	if got := m.switcher.contexts[m.switcher.cursor]; got != "office" {
		t.Fatalf("cursor on %q, want office", got)
	}
	if m.switcher.current != "office" {
		t.Fatalf("current = %q, want office", m.switcher.current)
	}
}

// An unset or stale current_context must still open on a valid row rather
// than out of range: nothing runs without enter, so row 0 is harmless.
func TestOpenSwitchModalUnknownCurrentContext(t *testing.T) {
	cfg := switchTestConfig()
	cfg.CurrentContext = "gone"

	model, _ := newObserveModel(cfg, "").openSwitchModal()
	m := asObserve(t, model)

	if m.switcher.cursor != 0 {
		t.Fatalf("cursor = %d, want 0", m.switcher.cursor)
	}
	if m.switcher.current != "gone" {
		t.Fatalf("current = %q, want gone", m.switcher.current)
	}
}

func TestOpenSwitchModalWithoutContexts(t *testing.T) {
	model, _ := newObserveModel(Config{}, "").openSwitchModal()
	m := asObserve(t, model)

	if m.switcher.open {
		t.Fatal("modal opened with no contexts configured")
	}
	if !m.messageIsErr || m.message == "" {
		t.Fatalf("no error message, got %q", m.message)
	}
}

func TestSwitchModalCursorMovesAndCancels(t *testing.T) {
	model, _ := newObserveModel(switchTestConfig(), "").openSwitchModal()

	model, _ = asObserve(t, model).handleSwitchKey(tea.KeyMsg{Type: tea.KeyUp})
	if got := asObserve(t, model).switcher.cursor; got != 0 {
		t.Fatalf("cursor = %d, want 0 after up", got)
	}

	model, _ = asObserve(t, model).handleSwitchKey(tea.KeyMsg{Type: tea.KeyUp})
	if got := asObserve(t, model).switcher.cursor; got != 0 {
		t.Fatalf("cursor = %d, want to stay clamped at 0", got)
	}

	model, _ = asObserve(t, model).handleSwitchKey(tea.KeyMsg{Type: tea.KeyEsc})
	if asObserve(t, model).switcher.open {
		t.Fatal("esc did not cancel the picker")
	}
}

// Closing must invalidate the run that was in flight, or its queued output
// lands in whatever the modal shows next.
func TestCloseSwitchModalInvalidatesInFlightRun(t *testing.T) {
	m := newObserveModel(switchTestConfig(), "")
	m.switcher = switchModal{open: true, phase: switchPhaseRun, seq: 4, selected: "home"}

	model, _ := m.closeSwitchModal()
	closed := asObserve(t, model)
	if closed.switcher.open {
		t.Fatal("modal still open")
	}
	if closed.switcher.seq != 5 {
		t.Fatalf("seq = %d, want 5", closed.switcher.seq)
	}

	stale := switchEventMsg{seq: 4, event: switchEvent{line: "late"}, ok: true}
	model, _ = closed.handleSwitchEvent(stale)
	if got := len(asObserve(t, model).switcher.lines); got != 0 {
		t.Fatalf("stale event appended %d lines", got)
	}
}

// A switch rewrites DNS, the proxy and several daemons in sequence; a key
// press must not be able to interrupt or hide it halfway through.
func TestSwitchModalRunPhaseIgnoresKeys(t *testing.T) {
	m := newObserveModel(switchTestConfig(), "")
	m.switcher = switchModal{open: true, phase: switchPhaseRun, seq: 1, selected: "home"}

	for _, key := range []tea.KeyMsg{
		{Type: tea.KeyEsc},
		{Type: tea.KeyEnter},
		{Type: tea.KeyRunes, Runes: []rune{'q'}},
	} {
		model, _ := m.handleSwitchKey(key)
		if !asObserve(t, model).switcher.open {
			t.Fatalf("key %v closed the modal mid-switch", key)
		}
	}
}

func TestSwitchModalDoneClosesOnEnter(t *testing.T) {
	m := newObserveModel(switchTestConfig(), "")
	m.switcher = switchModal{open: true, phase: switchPhaseDone, seq: 1, selected: "home"}

	model, _ := m.handleSwitchKey(tea.KeyMsg{Type: tea.KeyEnter})
	if asObserve(t, model).switcher.open {
		t.Fatal("enter did not close the finished modal")
	}
}

func TestHandleSwitchEventAppendsThenFinishes(t *testing.T) {
	m := newObserveModel(switchTestConfig(), "")
	events := make(chan switchEvent, 1)
	m.switcher = switchModal{
		open:     true,
		phase:    switchPhaseRun,
		seq:      2,
		selected: "home",
		viewport: newLogViewport(40, 5),
		events:   events,
	}

	model, cmd := m.handleSwitchEvent(switchEventMsg{seq: 2, event: switchEvent{line: "step one"}, ok: true})
	m = asObserve(t, model)
	if cmd == nil {
		t.Fatal("no follow-up read scheduled, so the stream would stall")
	}
	if len(m.switcher.lines) != 1 || m.switcher.lines[0] != "step one" {
		t.Fatalf("lines = %v", m.switcher.lines)
	}

	model, _ = m.handleSwitchEvent(switchEventMsg{seq: 2, event: switchEvent{done: true}, ok: true})
	m = asObserve(t, model)
	if m.switcher.phase != switchPhaseDone {
		t.Fatalf("phase = %v, want done", m.switcher.phase)
	}
	if m.switcher.events != nil {
		t.Fatal("events channel still referenced after the run finished")
	}
	if m.switcher.err != nil {
		t.Fatalf("err = %v, want nil", m.switcher.err)
	}
}

// A producer that dies without sending done would otherwise leave the modal
// stuck in the run phase, which ignores every key.
func TestHandleSwitchEventClosedChannelFinishes(t *testing.T) {
	m := newObserveModel(switchTestConfig(), "")
	m.switcher = switchModal{open: true, phase: switchPhaseRun, seq: 1, viewport: newLogViewport(40, 5)}

	model, _ := m.handleSwitchEvent(switchEventMsg{seq: 1, ok: false})
	if got := asObserve(t, model).switcher.phase; got != switchPhaseDone {
		t.Fatalf("phase = %v, want done", got)
	}
}

func TestStreamSwitchOutputSplitsLines(t *testing.T) {
	events := make(chan switchEvent, 8)
	streamSwitchOutput(strings.NewReader("first\r\nsecond\n"), events)
	close(events)

	var got []string
	for event := range events {
		got = append(got, event.line)
	}
	if len(got) != 2 || got[0] != "first" || got[1] != "second" {
		t.Fatalf("got %q", got)
	}
}

// h is the halt key now, and S must no longer stop a daemon. The cursor is
// moved off alpaca first: halting a connectivity-critical daemon goes through
// the confirmation prompt, which observe_confirm_test.go covers - this test
// is about which key maps to which action.
func TestObserveKeyBindings(t *testing.T) {
	stubLaunchdActions(t)
	m := newObserveModel(switchTestConfig(), "")
	m.cursor = rowIndexByName(t, m, appVPN)

	_, cmd := m.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'h'}})
	if cmd == nil {
		t.Fatal("h produced no action")
	}
	result := asActionResult(t, cmd())
	if result.verb != actionStop {
		t.Fatalf("h ran verb %q, want %q", result.verb, actionStop)
	}

	model, _ := m.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'S'}})
	if !asObserve(t, model).switcher.open {
		t.Fatal("S did not open the switch modal")
	}
}

func TestDisplayVerbRenamesStopToHalt(t *testing.T) {
	if got := displayVerb(actionStop); got != "halt" {
		t.Fatalf("displayVerb(stop) = %q, want halt", got)
	}
	if got := displayVerb(actionRestart); got != actionRestart {
		t.Fatalf("displayVerb(restart) = %q, want restart", got)
	}
}

func TestSwitchModalViewMarksCurrentContext(t *testing.T) {
	model, _ := newObserveModel(switchTestConfig(), "").openSwitchModal()
	m := asObserve(t, model)
	m.width, m.height = 100, 30

	view := m.switchModalView()
	for _, name := range []string{"home", "office", "remote"} {
		if !strings.Contains(view, name) {
			t.Fatalf("view is missing context %q:\n%s", name, view)
		}
	}
	if !strings.Contains(view, "(current)") {
		t.Fatalf("view does not mark the active context:\n%s", view)
	}
}

// rowIndexByName finds a daemon row so the tests do not hard-code the
// display order, which is a presentation decision and free to change.
func rowIndexByName(t *testing.T, m observeModel, name string) int {
	t.Helper()
	for i, row := range m.rows {
		if row.name == name {
			return i
		}
	}
	t.Fatalf("no row named %q", name)
	return -1
}

// asObserve unwraps the tea.Model that Update and the key handlers return.
// The assertion is checked so a handler that ever returns a different model
// fails loudly here instead of panicking somewhere further down the test.
func asObserve(t *testing.T, model tea.Model) observeModel {
	t.Helper()
	m, ok := model.(observeModel)
	if !ok {
		t.Fatalf("got %T, want observeModel", model)
	}
	return m
}

func asActionResult(t *testing.T, msg tea.Msg) actionResultMsg {
	t.Helper()
	result, ok := msg.(actionResultMsg)
	if !ok {
		t.Fatalf("got %T, want actionResultMsg", msg)
	}
	return result
}

// launchdCall records an action a test triggered, instead of performing it.
type launchdCall struct {
	verb  string
	label string
	scope string
}

// stubLaunchdActions cuts every observe action off from launchd for the
// duration of the test.
//
// This is mandatory, not optional hygiene. The alpaca row's label comes from
// launchAgentService(), so it is macswitcher's own real launch agent
// regardless of the Config a test constructs; executing the tea.Cmd that
// actionCmd returns would run `launchctl bootout` against it and take the
// machine off the network for the person running `go test`.
func stubLaunchdActions(t *testing.T) *[]launchdCall {
	t.Helper()
	calls := new([]launchdCall)
	original := launchdActions
	stubbed := make(map[string]func(label, scope string) error, len(original))
	for verb := range original {
		stubbed[verb] = func(label, scope string) error {
			*calls = append(*calls, launchdCall{verb: verb, label: label, scope: scope})
			return nil
		}
	}
	launchdActions = stubbed
	t.Cleanup(func() { launchdActions = original })
	return calls
}

// The stub must cover every verb actionCmd can dispatch, or the one it
// misses reaches the real launchd.
func TestStubLaunchdActionsCoversEveryVerb(t *testing.T) {
	for _, verb := range []string{actionStart, actionStop, actionRestart, actionEnable, actionDisable} {
		if _, ok := launchdActions[verb]; !ok {
			t.Errorf("launchdActions has no entry for %q", verb)
		}
	}
}
