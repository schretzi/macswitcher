package app

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// daemonStatus is the point-in-time launchd state of one observed agent.
type daemonStatus struct {
	Label        string
	Scope        string
	PlistPath    string
	Installed    bool // plist file exists at the standard path for its scope
	Loaded       bool // bootstrapped into launchd (launchctl print succeeds)
	Running      bool // has a live pid right now
	PID          int
	StartedAt    time.Time // process start time, derived from the live pid
	Runs         int       // launchd "runs" counter: a proxy for restart count since load
	LastExitCode int
	Err          error // set when the daemon can't be inspected (e.g. not configured)
}

const (
	daemonScopeUser   = "user"
	daemonScopeSystem = "system"
)

var (
	pidPattern      = regexp.MustCompile(`(?m)^\s*pid = (\d+)`)
	runsPattern     = regexp.MustCompile(`(?m)^\s*runs = (\d+)`)
	lastExitPattern = regexp.MustCompile(`(?m)^\s*last exit code = (-?\d+)`)
)

// serviceDomain and runCommandOutput are defined in service.go and reused here.

// normalizeDaemonScope maps a config Scope value (case-insensitive, empty
// meaning the default) to one of daemonScopeUser or daemonScopeSystem.
func normalizeDaemonScope(scope string) string {
	if strings.EqualFold(strings.TrimSpace(scope), daemonScopeSystem) {
		return daemonScopeSystem
	}
	return daemonScopeUser
}

// agentDomain returns the launchctl domain for scope: "system" for a
// LaunchDaemon, or the current user's gui/<uid> domain for a LaunchAgent.
func agentDomain(scope string) string {
	if normalizeDaemonScope(scope) == daemonScopeSystem {
		return daemonScopeSystem
	}
	return serviceDomain()
}

// agentPlistPath returns the standard plist path for label at scope:
// /Library/LaunchDaemons for "system", ~/Library/LaunchAgents for "user".
func agentPlistPath(scope, label string) (string, error) {
	if normalizeDaemonScope(scope) == daemonScopeSystem {
		return filepath.Join("/Library", "LaunchDaemons", label+".plist"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "Library", "LaunchAgents", label+".plist"), nil
}

// inspectDaemon queries launchd (and, if running, the process table) for the
// current state of the launch agent/daemon identified by label at scope. An
// empty label means the daemon is not configured; a missing plist means it
// is not installed; neither is treated as a hard error by callers. Reading
// launchd state never requires elevated privileges, even for scope=system.
func inspectDaemon(label, scope string) daemonStatus {
	status := daemonStatus{Label: label, Scope: normalizeDaemonScope(scope)}
	if strings.TrimSpace(label) == "" {
		status.Err = errors.New("not configured")
		return status
	}
	if plistPath, err := agentPlistPath(scope, label); err == nil {
		status.PlistPath = plistPath
		if _, err := os.Stat(plistPath); err == nil {
			status.Installed = true
		}
	}
	out, err := runCommandOutput("launchctl", "print", agentDomain(scope)+"/"+label)
	if err != nil {
		// Not bootstrapped (or unknown label): a normal, common state.
		return status
	}
	status.Loaded = true
	status.Running = strings.Contains(out, "state = running")
	if m := pidPattern.FindStringSubmatch(out); m != nil {
		status.PID, _ = strconv.Atoi(m[1])
	}
	if m := runsPattern.FindStringSubmatch(out); m != nil {
		status.Runs, _ = strconv.Atoi(m[1])
	}
	if m := lastExitPattern.FindStringSubmatch(out); m != nil {
		status.LastExitCode, _ = strconv.Atoi(m[1])
	}
	if status.PID > 0 {
		if startedOut, err := runCommandOutput("ps", "-o", "lstart=", "-p", strconv.Itoa(status.PID)); err == nil {
			if t, err := time.ParseInLocation("Mon Jan _2 15:04:05 2006", strings.TrimSpace(startedOut), time.Local); err == nil {
				status.StartedAt = t
			}
		}
	}
	return status
}

// launchdStart bootstraps (loads) label's launch agent. Only valid for
// scope=user; system-scoped actions require sudo and go through
// systemDaemonActionCmd instead.
func launchdStart(label, scope string) error {
	plistPath, err := agentPlistPath(scope, label)
	if err != nil {
		return err
	}
	if _, err := os.Stat(plistPath); err != nil {
		return fmt.Errorf("launch agent plist not found: %s", plistPath)
	}
	if _, err := runCommandOutput("launchctl", "bootstrap", agentDomain(scope), plistPath); err != nil {
		return err
	}
	return nil
}

// launchdStop boots out (unloads) label's launch agent, if currently loaded.
func launchdStop(label, scope string) error {
	plistPath, err := agentPlistPath(scope, label)
	if err != nil {
		return err
	}
	if _, err := runCommandOutput("launchctl", "bootout", agentDomain(scope), plistPath); err != nil {
		return err
	}
	return nil
}

// launchdRestart boots out and re-bootstraps label's launch agent. It
// tolerates the agent not being loaded yet.
func launchdRestart(label, scope string) error {
	_ = launchdStop(label, scope)
	return launchdStart(label, scope)
}

// launchdEnable clears any launchd-persisted "disabled" override for label,
// independent of whether it is currently loaded.
func launchdEnable(label, scope string) error {
	_, err := runCommandOutput("launchctl", "enable", agentDomain(scope)+"/"+label)
	return err
}

// launchdDisable sets a launchd-persisted "disabled" override for label, so
// it will not load at login even with RunAtLoad set, independent of whether
// it is currently loaded.
func launchdDisable(label, scope string) error {
	_, err := runCommandOutput("launchctl", "disable", agentDomain(scope)+"/"+label)
	return err
}

// systemDaemonActionCmd builds the sudo'd shell command for a mutating
// action (start/stop/restart/enable/disable) against a scope=system launch
// daemon. It is meant to be run interactively (e.g. via tea.ExecProcess) so
// sudo can prompt for a password on the real terminal.
func systemDaemonActionCmd(verb, label string) (*exec.Cmd, error) {
	plistPath, err := agentPlistPath(daemonScopeSystem, label)
	if err != nil {
		return nil, err
	}
	domain := daemonScopeSystem
	var shellCmd string
	switch verb {
	case "start":
		shellCmd = fmt.Sprintf("launchctl bootstrap %s %q", domain, plistPath)
	case "stop":
		shellCmd = fmt.Sprintf("launchctl bootout %s %q", domain, plistPath)
	case "restart":
		shellCmd = fmt.Sprintf("launchctl bootout %s %q; launchctl bootstrap %s %q", domain, plistPath, domain, plistPath)
	case "enable":
		shellCmd = fmt.Sprintf("launchctl enable %s/%s", domain, label)
	case "disable":
		shellCmd = fmt.Sprintf("launchctl disable %s/%s", domain, label)
	default:
		return nil, fmt.Errorf("unknown action %q", verb)
	}
	cmd := exec.Command("sudo", "sh", "-c", shellCmd) // #nosec G204 -- verb is one of a fixed set of internal actions; label/plistPath come from operator-controlled config, not untrusted input
	return cmd, nil
}

func expandTilde(path string) string {
	trimmed := strings.TrimSpace(path)
	if !strings.HasPrefix(trimmed, "~/") {
		return trimmed
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return trimmed
	}
	return filepath.Join(home, strings.TrimPrefix(trimmed, "~/"))
}

// kerberosTicketStatus summarizes the validity of a Kerberos credential
// cache file via klist. It never returns an error for a missing/expired
// ticket; those are reported through the returned summary/detail instead.
func kerberosTicketStatus(ticketFile string) (valid bool, summary string, detail string) {
	if strings.TrimSpace(ticketFile) == "" {
		return false, "not configured", "active context has no forwarder_proxy.ticket_file"
	}
	path := expandTilde(ticketFile)
	out, err := runCommandOutput("klist", "-c", path)
	if err != nil {
		msg := strings.TrimSpace(out)
		if msg == "" {
			msg = err.Error()
		}
		return false, "no valid ticket", msg
	}
	if strings.Contains(strings.ToLower(out), "expired") {
		return false, "expired", out
	}
	return true, "valid", out
}

// omtAccountStatus runs `omt status` and returns its output split into
// lines, plus a short "N/M valid" summary. It assumes the `omt` binary is
// on PATH; callers should check that separately via exec.LookPath.
func omtAccountStatus() (summary string, lines []string, err error) {
	out, err := runCommandOutput("omt", "status")
	if err != nil {
		return "", nil, err
	}
	all := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(all) == 0 {
		return "no accounts", nil, nil
	}
	total := 0
	valid := 0
	for _, line := range all[1:] { // skip header row
		if strings.TrimSpace(line) == "" {
			continue
		}
		total++
		fields := strings.Fields(line)
		for _, f := range fields {
			if strings.EqualFold(f, "valid") {
				valid++
				break
			}
		}
	}
	return fmt.Sprintf("%d/%d accounts valid", valid, total), all, nil
}

func omtInstalled() bool {
	_, err := exec.LookPath("omt")
	return err == nil
}

var vpnInetPattern = regexp.MustCompile(`(?m)^\s*inet (\S+)`)

// vpnInterfaceStatus reports whether iface (e.g. "utun99") currently exists
// and has an IPv4 address, which is the actual signal that a VPN tunnel is
// up — a running supervisor process only means openconnect hasn't crashed,
// not that the tunnel negotiated successfully.
func vpnInterfaceStatus(iface string) (up bool, detail string) {
	out, err := runCommandOutput("ifconfig", iface)
	if err != nil {
		return false, fmt.Sprintf("interface %s not present", iface)
	}
	if m := vpnInetPattern.FindStringSubmatch(out); m != nil {
		return true, fmt.Sprintf("interface %s up, inet %s", iface, m[1])
	}
	return false, fmt.Sprintf("interface %s present but has no address", iface)
}

var (
	knownDaemonKeys   = map[string]bool{"unbound": true, "kerberos_keep_alive": true, "omt": true, "vpn": true}
	knownDaemonFields = map[string]bool{"label": true, "scope": true, "interface": true}
)

// daemonsConfigWarnings re-reads path's raw YAML looking for typos under the
// top-level daemons: block (e.g. an unrecognized daemon name, or a field
// other than label/scope) that viper's non-strict decode would otherwise
// silently ignore, leaving the corresponding DaemonConfig zero-valued.
func daemonsConfigWarnings(path string) ([]string, error) {
	raw, err := os.ReadFile(path) // #nosec G304 -- path is the operator-provided config path, not untrusted input
	if err != nil {
		return nil, err
	}
	var doc map[string]any
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	daemonsRaw, ok := doc["daemons"]
	if !ok {
		return nil, nil
	}
	daemonsMap, ok := daemonsRaw.(map[string]any)
	if !ok {
		return []string{"daemons must be a mapping of daemon name to {label, scope}"}, nil
	}
	var warnings []string
	for key, val := range daemonsMap {
		if !knownDaemonKeys[key] {
			warnings = append(warnings, fmt.Sprintf(
				"daemons.%s is not a recognized daemon (expected one of unbound, kerberos_keep_alive, omt); it will be ignored",
				key,
			))
			continue
		}
		entry, ok := val.(map[string]any)
		if !ok {
			warnings = append(warnings, fmt.Sprintf("daemons.%s must be a mapping of {label, scope}", key))
			continue
		}
		for field := range entry {
			if !knownDaemonFields[field] {
				warnings = append(warnings, fmt.Sprintf(
					"daemons.%s.%s is not a recognized field (expected label or scope); it will be ignored",
					key, field,
				))
			}
		}
	}
	sort.Strings(warnings)
	return warnings, nil
}
