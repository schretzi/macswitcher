package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/schretzi/macswitcher/internal/service"

	"github.com/spf13/cobra"
)

// launchAgentService describes macswitcher's own launchd job: the foreground
// process it runs is `macswitcher proxy run`.
//
// "service" means this job and nothing else. The daemons macswitcher merely
// *observes* (alpaca, unbound, omt, kerberoskeepalive) are a separate concept,
// surfaced by `macswitcher observe`.
func launchAgentService() *service.Service {
	return service.New(appName, "proxy", "run")
}

// serviceCommand builds the `service` subtree from the shared implementation,
// so macswitcher, kerberoskeepalive and omt expose an identical surface.
func serviceCommand() *cobra.Command {
	cmd := service.NewCommand(launchAgentService(), prepareService)
	cmd.GroupID = "service"
	return cmd
}

// prepareService validates the config and resolves the alpaca binary, both of
// which have to happen after flag parsing rather than while the command tree
// is being built.
func prepareService(s *service.Service) error {
	path, err := configuredPath()
	if err != nil {
		return err
	}
	if err := configValidate(path); err != nil {
		return fmt.Errorf("config invalid: %w", err)
	}

	// launchd starts jobs with a minimal PATH, so `alpaca` has to be recorded
	// as an absolute path at install time rather than looked up at run time.
	alpaca, err := exec.LookPath(appAlpaca)
	if err != nil {
		return fmt.Errorf("alpaca executable not found in PATH: %w", err)
	}
	s.WithEnv("MACSWITCHER_ALPACA_BINARY", alpaca)
	return nil
}

// serviceStop, serviceRestart and serviceStatus are used by `switch`, which
// stops the proxy when moving to a proxy-off context and restarts it
// otherwise, and by `status`.

func serviceStop() error { return launchAgentService().Stop() }

func serviceRestart() error { return launchAgentService().Restart() }

func serviceStatus() error {
	st, err := launchAgentService().Status()
	if err != nil {
		return err
	}
	service.WriteStatus(os.Stdout, st)
	return nil
}

// commandTimeout bounds the short-lived helper commands macswitcher shells
// out to (networksetup, launchctl, dscacheutil, ...). They normally return in
// milliseconds; a wedged one used to hang a context switch indefinitely.
const commandTimeout = 30 * time.Second

// keychainTimeout is longer than commandTimeout because `security` can block
// on a user-facing unlock prompt.
const keychainTimeout = 2 * time.Minute

func runCommand(name string, args ...string) error {
	ctx, cancel := context.WithTimeout(context.Background(), commandTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, name, args...) // #nosec G204 -- args are macswitcher-internal command definitions from trusted config/system calls, not raw user input
	cmd.Stdout = nil
	cmd.Stderr = nil
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("command failed: %s %s: %w", name, strings.Join(args, " "), err)
	}
	return nil
}

func runCommandList(parts []string) error {
	if len(parts) == 0 {
		return errors.New("empty command")
	}
	return runCommand(parts[0], parts[1:]...)
}

func runCommandOutput(name string, args ...string) (string, error) {
	return runCommandOutputTimeout(commandTimeout, name, args...)
}

// runCommandOutputTimeout is runCommandOutput with the timeout spelled out, for
// callers that poll: a probe repeated on a schedule has to give up well inside
// commandTimeout or the retry never gets a second attempt.
func runCommandOutputTimeout(timeout time.Duration, name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, name, args...) // #nosec G204 -- args are macswitcher-internal command definitions from trusted config/system calls, not raw user input
	b, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("command failed: %s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(b)))
	}
	return strings.TrimSpace(string(b)), nil
}

// serviceDomain is the launchctl domain for the current user's GUI session.
// daemons.go and observe.go reuse it when inspecting *other* people's agents.
func serviceDomain() string {
	return fmt.Sprintf("gui/%d", os.Getuid())
}
