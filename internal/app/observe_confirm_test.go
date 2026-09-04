package app

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

func confirmTestModel(t *testing.T) observeModel {
	t.Helper()
	// Without this the alpaca row would act on macswitcher's own live launch
	// agent: its label does not come from this Config.
	stubLaunchdActions(t)
	cfg := Config{Daemons: map[string]DaemonConfig{
		appAdGuard: {Label: "com.example.adguardhome"},
		appPrivoxy: {Label: "com.example.privoxy"},
		appVPN:     {Label: "com.example.vpn"},
	}}
	m := newObserveModel(cfg, "")
	m.cursor = rowIndexByName(t, m, appAlpaca)
	return m
}

// The cursor starts on row 0, and row 0 is alpaca, so an unmodified single
// keystroke used to be enough to disconnect the machine on a freshly opened
// TUI. This is the regression that guards against it.
func TestHaltOnAlpacaAsksBeforeActing(t *testing.T) {
	m := confirmTestModel(t)

	model, cmd := m.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'h'}})
	got := asObserve(t, model)

	if cmd != nil {
		t.Fatal("halt ran immediately instead of asking")
	}
	if got.pending == nil {
		t.Fatal("no confirmation pending")
	}
	if got.pending.verb != actionStop {
		t.Fatalf("pending verb = %q, want %q", got.pending.verb, actionStop)
	}
	if !strings.Contains(got.message, appAlpaca) || !strings.Contains(got.message, "press y") {
		t.Fatalf("prompt does not name the daemon and the key: %q", got.message)
	}
}

func TestAlpacaHaltProceedsOnY(t *testing.T) {
	m := confirmTestModel(t)
	model, _ := m.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'h'}})

	model, cmd := asObserve(t, model).handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'y'}})
	got := asObserve(t, model)

	if got.pending != nil {
		t.Fatal("confirmation still pending after y")
	}
	if cmd == nil {
		t.Fatal("y did not run the action")
	}
	result := asActionResult(t, cmd())
	if result.verb != actionStop {
		t.Fatalf("ran verb %q, want %q", result.verb, actionStop)
	}
}

// Anything other than y is a no. A confirmation prompt that only treats "n"
// as refusal would let a stray keystroke through, which is the exact failure
// mode being defended against.
func TestAlpacaHaltCancelsOnAnyOtherKey(t *testing.T) {
	for _, key := range []tea.KeyMsg{
		{Type: tea.KeyRunes, Runes: []rune{'n'}},
		{Type: tea.KeyEsc},
		{Type: tea.KeyEnter},
		{Type: tea.KeyRunes, Runes: []rune{'h'}},
		{Type: tea.KeyRunes, Runes: []rune{'Y'}},
	} {
		m := confirmTestModel(t)
		model, _ := m.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'h'}})

		model, cmd := asObserve(t, model).handleKey(key)
		got := asObserve(t, model)

		if got.pending != nil {
			t.Fatalf("key %v left a confirmation pending", key)
		}
		if cmd != nil {
			t.Fatalf("key %v ran the action instead of cancelling", key)
		}
		if !strings.Contains(got.message, "cancelled") {
			t.Fatalf("key %v: message = %q, want a cancellation", key, got.message)
		}
	}
}

// esc normally quits the TUI. While a confirmation is up it must mean "no",
// not "no, and also close the window", or the answer is ambiguous.
func TestConfirmationSwallowsQuitKeys(t *testing.T) {
	m := confirmTestModel(t)
	model, _ := m.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'h'}})

	model, _ = asObserve(t, model).handleKey(tea.KeyMsg{Type: tea.KeyEsc})
	if asObserve(t, model).quitting {
		t.Fatal("esc quit the TUI while answering a confirmation")
	}
}

// The prompt names one daemon; the action must hit that one even if the
// cursor moved in between - it cannot, because the prompt swallows the
// movement key, but the index is pinned rather than relying on that.
func TestConfirmationPinsTheRowItAskedAbout(t *testing.T) {
	m := confirmTestModel(t)
	alpaca := m.cursor

	model, _ := m.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'h'}})
	moved := asObserve(t, model)
	moved.cursor = len(moved.rows) - 1

	model, cmd := moved.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'y'}})
	if got := asObserve(t, model).cursor; got != alpaca {
		t.Fatalf("cursor = %d, want %d (the row that was confirmed)", got, alpaca)
	}
	if result := asActionResult(t, cmd()); result.index != alpaca {
		t.Fatalf("acted on row %d, want %d", result.index, alpaca)
	}
}

// Restart ends with the daemon running, so it is not the failure mode the
// prompt exists for, and making it ask would train people to hit y reflexively.
func TestRestartAndStartDoNotAsk(t *testing.T) {
	for _, key := range []rune{'R', 's'} {
		m := confirmTestModel(t)
		model, cmd := m.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{key}})
		if asObserve(t, model).pending != nil {
			t.Fatalf("key %q asked for confirmation", string(key))
		}
		if cmd == nil {
			t.Fatalf("key %q did nothing", string(key))
		}
	}
}

// disable outlives a reboot, which is how a machine comes back up still
// disconnected, so it is confirmed exactly like halt.
func TestDisableOnCriticalDaemonAsks(t *testing.T) {
	m := confirmTestModel(t)
	model, cmd := m.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'d'}})
	if cmd != nil {
		t.Fatal("disable ran immediately instead of asking")
	}
	if got := asObserve(t, model); got.pending == nil || got.pending.verb != actionDisable {
		t.Fatalf("pending = %+v, want a disable confirmation", got.pending)
	}
}

// Only the daemons that carry DNS and outbound HTTP are worth interrupting
// for; a prompt on every row is a prompt nobody reads.
func TestNonCriticalDaemonHaltsWithoutAsking(t *testing.T) {
	m := confirmTestModel(t)
	m.cursor = rowIndexByName(t, m, appVPN)

	model, cmd := m.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'h'}})
	if asObserve(t, model).pending != nil {
		t.Fatal("a non-critical daemon asked for confirmation")
	}
	if cmd == nil {
		t.Fatal("halt did nothing")
	}
}

func TestCriticalDaemonsCoverTheNetworkPath(t *testing.T) {
	for _, name := range []string{appAlpaca, appAdGuard, appPrivoxy} {
		if !criticalDaemons[name] {
			t.Errorf("%s is not marked critical", name)
		}
		if criticalDaemonWarning(name) == "" {
			t.Errorf("%s has no warning text", name)
		}
	}
}

func TestIsDestructiveVerb(t *testing.T) {
	for verb, want := range map[string]bool{
		actionStop:    true,
		actionDisable: true,
		actionStart:   false,
		actionRestart: false,
		actionEnable:  false,
	} {
		if got := isDestructiveVerb(verb); got != want {
			t.Errorf("isDestructiveVerb(%q) = %v, want %v", verb, got, want)
		}
	}
}
