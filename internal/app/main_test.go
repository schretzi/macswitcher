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
//
// dnsServiceOps is armed for the same reason and is, if anything, worse:
// preflight's DHCP fallback rewrites the resolvers of EVERY network service
// and relies on a defer to put them back. A test reaching that would
// reconfigure the developer's DNS, and a test that then failed an assertion
// mid-way could leave it reconfigured. Tests must call stubDNSServiceOps.
func TestMain(m *testing.M) {
	launchdActions = armedLaunchdActions()
	dnsServiceOps = armedDNSServiceOps()
	os.Exit(m.Run())
}

func armedDNSServiceOps() dnsOps {
	return dnsOps{
		get: func(service string) ([]string, error) {
			panic(fmt.Sprintf(
				"test tried to read the DNS servers of %q; call stubDNSServiceOps(t) first",
				service,
			))
		},
		set: func(service string, resolvers []string) error {
			panic(fmt.Sprintf(
				"test tried to set the DNS servers of %q to %v; call stubDNSServiceOps(t) first",
				service, resolvers,
			))
		},
		flush: func() {
			panic("test tried to flush the system DNS cache; call stubDNSServiceOps(t) first")
		},
		resolve: func(host string) error {
			panic(fmt.Sprintf(
				"test tried to resolve %q against the real network; call stubDNSServiceOps(t) first",
				host,
			))
		},
	}
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
