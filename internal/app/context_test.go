package app

import "testing"

// rollbackContext re-applies a whole context, which touches DNS, launchd and
// the system proxy. Its guard clauses are what keep it from doing any of that
// when there is nothing sensible to roll back to - a first-ever switch, or a
// previous context that has since been removed from the config. Both have to
// return without applying anything.
func TestRollbackContextDoesNothingWithoutAUsablePreviousContext(t *testing.T) {
	t.Parallel()

	cfg := Config{Contexts: map[string]SwitchContext{
		"home": {ProxyMode: ProxyModeDirect},
	}}

	tests := []struct {
		name     string
		previous string
		failed   string
	}{
		{name: "no previous context recorded", previous: "", failed: "home"},
		{name: "previous context is only whitespace", previous: "   ", failed: "home"},
		{name: "previous context no longer configured", previous: "retired", failed: "home"},
		{name: "previous context is the one that failed", previous: "home", failed: "home"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			// An empty cfgPath would fail loudly if applyContext were reached,
			// since it is where the runtime state would be written.
			rollbackContext("", cfg, tt.previous, tt.failed)
		})
	}
}
