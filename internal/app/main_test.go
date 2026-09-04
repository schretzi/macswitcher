package app

import (
	"fmt"
	"os"
	"testing"
)

// TestMain disarms the launchd action table for the whole test binary.
//
// observe's alpaca row takes its label from launchAgentService(), not from
// the Config a test builds, so it always names macswitcher's own live launch
// agent. Executing the tea.Cmd that actionCmd returns therefore ran
// `launchctl bootout` against the operator's actual proxy: every `go test`
// run took the machine off the network, and the failure was invisible in the
// test output because bootout succeeds.
//
// A test that means to exercise an action must call stubLaunchdActions and
// assert on the recorded calls. Anything else panics here, loudly and before
// touching the system, rather than quietly disconnecting the developer.
func TestMain(m *testing.M) {
	launchdActions = armedLaunchdActions()
	os.Exit(m.Run())
}

func armedLaunchdActions() map[string]func(label, scope string) error {
	actions := make(map[string]func(label, scope string) error, len(launchdActions))
	for verb := range launchdActions {
		actions[verb] = func(label, scope string) error {
			panic(fmt.Sprintf(
				"test tried to run launchctl %s on %q (scope %s); call stubLaunchdActions(t) first",
				verb, label, scope,
			))
		}
	}
	return actions
}
