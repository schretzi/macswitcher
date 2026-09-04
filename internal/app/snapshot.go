package app

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/schretzi/macswitcher/internal/version"
)

// A snapshot is what you take when the network has just gone away and you
// cannot look anything up, cannot ask anyone, and cannot run the commands you
// would normally run because half of them want to resolve a name.
//
// It answers one question - "what state is this machine actually in?" - and
// writes the answer somewhere that survives closing the terminal. Everything
// here is therefore built around three rules:
//
//  1. It never changes anything. Not the resolvers, not a daemon, not the
//     config. A diagnostic that mutates is a diagnostic nobody dares run at
//     the moment it is needed.
//  2. It never blocks for long. Every external command is bounded well below
//     the usual timeout, because the whole point is that the network is
//     broken and half these commands would otherwise sit waiting for it.
//  3. It never fails as a whole. A collector that errors records its error as
//     its content and the rest carries on. A partial snapshot is worth a great
//     deal; an aborted one is worth nothing.

const (
	// snapshotCommandTimeout is deliberately shorter than commandTimeout.
	// These commands run on a machine whose network is already known to be
	// broken, and there are about twenty of them: at 30s each a snapshot
	// could take ten minutes to tell you what it already knew at second one.
	snapshotCommandTimeout = 5 * time.Second

	// snapshotKeep is how many snapshot directories to keep. They are small
	// but not free, nothing else prunes them, and newsyslog cannot rotate a
	// directory - so the command that creates them is the only thing in a
	// position to clean them up.
	snapshotKeep = 20

	// snapshotTailLines is much shorter than the TUI's tailLines. The log
	// viewer is scrollable, so 2000 lines there costs nothing; here they are
	// twenty files written to disk, and kanata alone would contribute 2000
	// copies of "virtual_hid_keyboard_ready true" - a megabyte of a snapshot
	// spent on a line nobody will read. What matters at a failure is the last
	// few minutes.
	snapshotTailLines = 300

	// snapshotDetailInline is how much of a daemon's detail goes in the
	// report table. omt lists one row per account and blows a markdown cell
	// well past readable; the full text is in the daemon's own file, which is
	// where anyone who needs all of it will look anyway.
	snapshotDetailInline = 120
)

type snapshotRequest struct {
	// Reason is one line on why this snapshot exists: the failed switch, or
	// whatever the operator typed into --reason. It is the first thing in the
	// report, because a directory full of timestamps is otherwise a puzzle.
	Reason string
	// Journal is the switch that just failed, when there is one. nil for a
	// snapshot taken by hand, which then falls back to the switch log on disk.
	Journal *switchJournal
}

// writeSnapshot collects everything and writes it under a fresh directory.
//
// A DIRECTORY, not a single file, and the question is worth answering because
// the obvious choice is the wrong one. A snapshot is a handful of kilobytes of
// summary plus a few hundred of raw output - daemon logs alone are capped at
// 2000 lines each. Flattened into one file the summary drowns: the first thing
// you want to read is buried under `netstat -rn`. As a directory the summary is
// a file of its own, the raw material sits next to it under names that say what
// it is, and the whole thing is still one unit to `tar czf` and attach to a
// message. `report.md` is written to stand alone, so "just read the file" is
// still available - it is simply not the only option.
func writeSnapshot(cfgPath string, cfg Config, req snapshotRequest) (string, error) {
	dir, err := snapshotDir(time.Now())
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return "", err
	}

	snap := collectSnapshot(cfgPath, cfg, req)

	if err := os.WriteFile(filepath.Join(dir, "report.md"), []byte(snap.report()), 0o600); err != nil {
		return "", err
	}
	for _, f := range snap.files {
		path := filepath.Join(dir, f.name)
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			continue
		}
		// Individual failures are not fatal: losing one raw file is much
		// better than losing the snapshot.
		_ = os.WriteFile(path, []byte(redact(f.content)), 0o600)
	}

	pruneSnapshots(snapshotKeep)
	return dir, nil
}

func snapshotDir(now time.Time) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	// Under one parent rather than as ~/Library/Logs/macswitcher_snapshot-*:
	// the log directory is shared with every other job on the machine, and
	// twenty sibling directories from one tool make the place unreadable for
	// everyone. One parent also gives pruning something to iterate.
	return filepath.Join(home, "Library", "Logs", "macswitcher-snapshots", now.Format("2006-01-02T15-04-05")), nil
}

// pruneSnapshots keeps the newest keep directories and removes the rest.
// Best effort throughout - a snapshot that failed to tidy up is still a good
// snapshot.
func pruneSnapshots(keep int) {
	home, err := os.UserHomeDir()
	if err != nil {
		return
	}
	parent := filepath.Join(home, "Library", "Logs", "macswitcher-snapshots")
	entries, err := os.ReadDir(parent)
	if err != nil {
		return
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			names = append(names, e.Name())
		}
	}
	// The directory name is a sortable timestamp, so lexical order is
	// chronological order - which is the reason for that format.
	sort.Strings(names)
	for i := range len(names) - keep {
		_ = os.RemoveAll(filepath.Join(parent, names[i]))
	}
}

type snapshotFile struct {
	name    string
	content string
}

type snapshot struct {
	takenAt time.Time
	reason  string
	cfgPath string
	from    string
	to      string
	// failedStep is empty for a snapshot that is not about a failed switch.
	failedStep string
	switchErr  string
	steps      []journalStep
	daemons    []snapshotDaemon
	probes     []snapshotProbe
	files      []snapshotFile
}

type snapshotDaemon struct {
	name   string
	status daemonStatus
	extra  []string
	logs   []string
}

type snapshotProbe struct {
	host string
	err  error
}

func (s *snapshot) add(name, content string) {
	s.files = append(s.files, snapshotFile{name: name, content: content})
}

// addCommand runs one read-only command and files its output. The command
// line is recorded above the output so the reader can reproduce it by hand,
// which is most of what makes raw output useful to somebody else.
func (s *snapshot) addCommand(name, bin string, args ...string) {
	out, err := runCommandOutputTimeout(snapshotCommandTimeout, bin, args...)
	var b strings.Builder
	fmt.Fprintf(&b, "$ %s %s\n\n", bin, strings.Join(args, " "))
	if err != nil {
		fmt.Fprintf(&b, "command failed: %v\n", err)
	}
	b.WriteString(out)
	b.WriteString("\n")
	s.add(name, b.String())
}

func (s *snapshot) addFileCopy(name, path string) {
	if strings.TrimSpace(path) == "" {
		return
	}
	b, err := os.ReadFile(path) // #nosec G304 -- paths come from macswitcher's own config, not from untrusted input
	if err != nil {
		s.add(name, fmt.Sprintf("# %s\n\ncould not read: %v\n", path, err))
		return
	}
	s.add(name, fmt.Sprintf("# %s\n\n%s", path, b))
}

func collectSnapshot(cfgPath string, cfg Config, req snapshotRequest) *snapshot {
	s := &snapshot{takenAt: time.Now(), reason: req.Reason, cfgPath: cfgPath}

	if j := req.Journal; j != nil {
		s.from, s.to = j.From, j.To
		s.failedStep = j.FailedStep()
		s.steps = j.Steps
		if j.Err != nil {
			s.switchErr = j.Err.Error()
		}
		s.add("switch.log", j.Render())
	} else {
		// Taken by hand, so the switch that went wrong already finished. The
		// running switch log is the only place its story still exists, and
		// without it a manual snapshot could not answer "from which context
		// to which" at all.
		s.from = cfg.CurrentContext
		s.add("switch-history.log", lastSwitchLogEntries(5))
	}

	s.collectMeta(cfg)
	s.collectDaemons(cfg, cfgPath)
	s.collectNetwork(cfg)
	s.collectProbes(cfg)
	s.collectConfig(cfg, cfgPath)
	return s
}

func (s *snapshot) collectMeta(cfg Config) {
	var b strings.Builder
	fmt.Fprintf(&b, "taken_at:        %s\n", s.takenAt.Format(time.RFC3339))
	fmt.Fprintf(&b, "reason:          %s\n", s.reason)
	fmt.Fprintf(&b, "macswitcher:     %s\n", version.String(appName))
	fmt.Fprintf(&b, "config:          %s\n", s.cfgPath)
	fmt.Fprintf(&b, "current_context: %s\n", cfg.CurrentContext)
	if host, err := os.Hostname(); err == nil {
		fmt.Fprintf(&b, "hostname:        %s\n", host)
	}
	s.add("meta.txt", b.String())
	// Uptime doubles as the answer to "did this machine just wake up?", which
	// is a common enough cause of a network that is present but not yet
	// working that it is worth one line.
	s.addCommand("system/uptime.txt", "uptime")
	s.addCommand("system/sw_vers.txt", "sw_vers")
}

// collectDaemons walks exactly the rows `observe` shows, so the snapshot and
// the TUI can never disagree about which daemons matter - including any added
// purely through config, like kanata.
func (s *snapshot) collectDaemons(cfg Config, cfgPath string) {
	rows := newObserveModel(cfg, cfgPath).rows
	for _, row := range rows {
		d := snapshotDaemon{
			name:   row.name,
			status: inspectDaemon(row.label, row.scope),
			extra:  gatherExtra(cfg, row),
		}
		for _, src := range daemonLogSources(row) {
			lines, err := readLogTail(src.path)
			if err != nil {
				d.logs = append(d.logs, fmt.Sprintf("%s (%s): %v", src.path, src.kind, err))
				continue
			}
			if len(lines) > snapshotTailLines {
				lines = lines[len(lines)-snapshotTailLines:]
			}
			d.logs = append(d.logs, fmt.Sprintf("%s (%s): %d lines", src.path, src.kind, len(lines)))
			s.add(
				filepath.Join("daemons", row.name+"."+src.kind.String()+".txt"),
				fmt.Sprintf("# %s (last %d lines)\n\n%s\n", src.path, len(lines), strings.Join(lines, "\n")),
			)
		}
		s.add(filepath.Join("daemons", row.name+".status.txt"), daemonStatusText(d))
		s.daemons = append(s.daemons, d)
	}
}

func (s *snapshot) collectNetwork(cfg Config) {
	// scutil --dns is the single most useful command here: it shows the
	// resolvers actually in effect per interface, including the ones a VPN
	// installed, which networksetup does not know about at all.
	s.addCommand("network/scutil-dns.txt", "scutil", "--dns")
	s.addCommand("network/network-location.txt", "scselect")
	s.addCommand("network/interfaces.txt", "ifconfig")
	s.addCommand("network/routes.txt", "netstat", "-rn")
	s.addCommand("network/listening-ports.txt", "netstat", "-an", "-p", "tcp")
	s.addCommand("network/services.txt", "networksetup", "-listallnetworkservices")

	services, err := resolveNetworkServices(cfg)
	if err != nil {
		s.add("network/per-service.txt", fmt.Sprintf("could not list network services: %v\n", err))
		return
	}
	var b strings.Builder
	for _, svc := range services {
		fmt.Fprintf(&b, "=== %s\n", svc)
		for _, q := range [][]string{
			{"-getdnsservers", svc},
			{"-getsearchdomains", svc},
			{"-getinfo", svc},
			// The system proxy settings, which is where a half-finished
			// switch most often leaves a contradiction: resolvers from one
			// context, proxy from the other.
			{"-getwebproxy", svc},
			{"-getsecurewebproxy", svc},
			{"-getautoproxyurl", svc},
		} {
			out, err := runCommandOutputTimeout(snapshotCommandTimeout, "networksetup", q...)
			fmt.Fprintf(&b, "  $ networksetup %s\n", strings.Join(q, " "))
			if err != nil {
				fmt.Fprintf(&b, "    failed: %v\n", err)
				continue
			}
			for line := range strings.SplitSeq(out, "\n") {
				fmt.Fprintf(&b, "    %s\n", line)
			}
		}
		b.WriteString("\n")
	}
	s.add("network/per-service.txt", b.String())
}

// collectProbes records what actually resolves, which is the difference
// between "DNS is broken" and "this one name is broken" - and that distinction
// is usually the whole diagnosis.
//
// It uses the same bounded single-shot probe the switch uses, so the snapshot
// reports what the switch would have seen rather than a second opinion from a
// different resolver path.
func (s *snapshot) collectProbes(cfg Config) {
	hosts := []string{"google.com"}
	if ctx, ok := cfg.Contexts[cfg.CurrentContext]; ok {
		hosts = append(preflightHosts(ctx), hosts...)
	}
	seen := map[string]bool{}
	var b strings.Builder
	for _, host := range hosts {
		if host == "" || seen[strings.ToLower(host)] {
			continue
		}
		seen[strings.ToLower(host)] = true
		// Through dnsServiceOps like everything else that resolves, so the
		// armed test stub covers this path too. A collector that reached the
		// real resolver would make `go test` depend on the developer's
		// network - the exact class of bug the arming exists to prevent.
		err := dnsServiceOps.resolve(host)
		s.probes = append(s.probes, snapshotProbe{host: host, err: err})
		if err != nil {
			fmt.Fprintf(&b, "%-40s FAILED: %v\n", host, err)
			continue
		}
		fmt.Fprintf(&b, "%-40s ok\n", host)
	}
	s.add("dns-probes.txt", b.String())
}

func (s *snapshot) collectConfig(cfg Config, cfgPath string) {
	s.addFileCopy("config/config.yaml", cfgPath)
	if dir := filepath.Dir(cfgPath); dir != "" {
		if entries, err := os.ReadDir(filepath.Join(dir, "contexts")); err == nil {
			for _, e := range entries {
				if !e.IsDir() {
					s.addFileCopy(filepath.Join("config", "contexts", e.Name()), filepath.Join(dir, "contexts", e.Name()))
				}
			}
		}
	}
	s.addFileCopy("config/adguard-upstreams.conf", strings.TrimSpace(cfg.AdGuard.UpstreamsFile))
	if home, err := os.UserHomeDir(); err == nil {
		// The generated PAC and the shell rc are what the rest of the system
		// actually reads. When they disagree with the config, that
		// disagreement is the bug.
		s.addFileCopy("config/filter.pac", filepath.Join(home, ".local", "state", "macswitcher", "filter.pac"))
		s.addFileCopy("config/zsh-proxy-rc", filepath.Join(home, ".zsh", "rcs", "proxy"))
	}
}

// credentialInURL matches the userinfo part of a URL - http://user:pass@host.
//
// macswitcher keeps its secrets in the Keychain and its config holds only
// references, so nothing here is expected to contain a password. "Expected" is
// the operative word: a snapshot exists to be sent to somebody else, possibly
// pasted into a chat, and a proxy URL with inline credentials is exactly the
// kind of thing that ends up in one by accident. Redacting costs one regex.
var credentialInURL = regexp.MustCompile(`([a-zA-Z][a-zA-Z0-9+.-]*://)[^/\s:@]+:[^/\s@]+@`)

func redact(content string) string {
	return credentialInURL.ReplaceAllString(content, "${1}REDACTED:REDACTED@")
}

// lastSwitchLogEntries returns the tail of the switch log, for a snapshot
// taken by hand after the fact.
func lastSwitchLogEntries(entries int) string {
	path, err := switchLogPath()
	if err != nil {
		return "no switch log path\n"
	}
	lines, err := readLogTail(path)
	if err != nil {
		return fmt.Sprintf("# %s\n\ncould not read: %v\n", path, err)
	}
	// Entries are separated by the "=== " header line, so counting those
	// backwards yields whole entries rather than a cut in the middle of one.
	starts := make([]int, 0, entries+1)
	for i, line := range lines {
		if strings.HasPrefix(line, "=== ") {
			starts = append(starts, i)
		}
	}
	if len(starts) > entries {
		lines = lines[starts[len(starts)-entries]:]
	}
	return fmt.Sprintf("# %s\n\n%s\n", path, strings.Join(lines, "\n"))
}

// report renders the one file somebody actually reads.
//
// Ordered by what a reader needs first: what was being attempted, where it
// stopped, then the state that resulted. The raw files are listed at the end
// rather than the start - they are the follow-up, not the answer.
func (s *snapshot) report() string {
	var b strings.Builder
	b.WriteString("# macswitcher snapshot\n\n")
	fmt.Fprintf(&b, "- **taken**: %s\n", s.takenAt.Format(time.RFC3339))
	if s.reason != "" {
		fmt.Fprintf(&b, "- **reason**: %s\n", s.reason)
	}
	fmt.Fprintf(&b, "- **version**: %s\n", version.String(appName))
	if s.to != "" {
		fmt.Fprintf(&b, "- **switch**: `%s` -> `%s`\n", orNone(s.from), s.to)
	} else {
		fmt.Fprintf(&b, "- **current context**: `%s`\n", orNone(s.from))
	}
	if s.failedStep != "" {
		fmt.Fprintf(&b, "- **failed at**: %s\n", s.failedStep)
	}
	if s.switchErr != "" {
		fmt.Fprintf(&b, "\n```\n%s\n```\n", s.switchErr)
	}

	s.reportSteps(&b)
	s.reportProbes(&b)
	s.reportDaemons(&b)
	s.reportFiles(&b)
	return redact(b.String())
}

func (s *snapshot) reportSteps(b *strings.Builder) {
	if len(s.steps) == 0 {
		return
	}
	b.WriteString("\n## What the switch did\n\n")
	b.WriteString("Steps above the failure took effect and were **not** undone.\n\n")
	b.WriteString("| # | step | result |\n|---|---|---|\n")
	for i, step := range s.steps {
		result := fmt.Sprintf("ok (%s)", step.Duration.Round(time.Millisecond))
		switch {
		case step.Skipped:
			result = "skipped (not configured for this context)"
		case step.Err != nil:
			result = "**FAILED**"
		}
		fmt.Fprintf(b, "| %d | %s | %s |\n", i+1, step.Name, result)
	}
}

func (s *snapshot) reportProbes(b *strings.Builder) {
	if len(s.probes) == 0 {
		return
	}
	b.WriteString("\n## DNS\n\n")
	for _, p := range s.probes {
		if p.err != nil {
			fmt.Fprintf(b, "- `%s` — **failed**\n", p.host)
			continue
		}
		fmt.Fprintf(b, "- `%s` — ok\n", p.host)
	}
	b.WriteString("\nSee `network/scutil-dns.txt` for the resolvers actually in effect,\n")
	b.WriteString("which is not always what `networksetup` reports.\n")
}

func (s *snapshot) reportDaemons(b *strings.Builder) {
	if len(s.daemons) == 0 {
		return
	}
	b.WriteString("\n## Daemons\n\n")
	b.WriteString("| daemon | state | pid | runs | last exit | detail |\n")
	b.WriteString("|---|---|---|---|---|---|\n")
	for _, d := range s.daemons {
		fmt.Fprintf(b, "| %s | %s | %s | %d | %d | %s |\n",
			d.name, daemonStateWord(d.status), pidOrDash(d.status),
			d.status.Runs, d.status.LastExitCode,
			inlineDetail(d.extra))
	}
	b.WriteString("\nFull detail and log tails per daemon are under `daemons/`.\n")
}

// inlineDetail keeps the table a table. A cell that runs to several hundred
// characters does not wrap in any markdown viewer worth the name; it pushes
// every other column off the screen, which costs more than the detail is worth
// at a glance.
func inlineDetail(extra []string) string {
	joined := strings.ReplaceAll(strings.Join(extra, "; "), "|", "/")
	joined = strings.Join(strings.Fields(joined), " ")
	if len(joined) <= snapshotDetailInline {
		return joined
	}
	return joined[:snapshotDetailInline] + "... (see daemons/)"
}

// daemonStatusText is the unabridged per-daemon record: everything the TUI
// would show for that row, in a file that has no column width to respect.
func daemonStatusText(d snapshotDaemon) string {
	var b strings.Builder
	fmt.Fprintf(&b, "name:       %s\n", d.name)
	fmt.Fprintf(&b, "label:      %s\n", d.status.Label)
	fmt.Fprintf(&b, "scope:      %s\n", d.status.Scope)
	fmt.Fprintf(&b, "plist:      %s\n", d.status.PlistPath)
	fmt.Fprintf(&b, "state:      %s\n", daemonStateWord(d.status))
	fmt.Fprintf(&b, "pid:        %s\n", pidOrDash(d.status))
	if !d.status.StartedAt.IsZero() {
		fmt.Fprintf(&b, "started_at: %s\n", d.status.StartedAt.Format(time.RFC3339))
	}
	// runs climbing with no pid is the signature of a crash loop, which is
	// why both are here rather than just the live state.
	fmt.Fprintf(&b, "runs:       %d\n", d.status.Runs)
	fmt.Fprintf(&b, "last_exit:  %d\n", d.status.LastExitCode)
	if d.status.Err != nil {
		fmt.Fprintf(&b, "error:      %v\n", d.status.Err)
	}
	if len(d.extra) > 0 {
		b.WriteString("\ndetail:\n")
		for _, line := range d.extra {
			fmt.Fprintf(&b, "  %s\n", line)
		}
	}
	if len(d.logs) > 0 {
		b.WriteString("\nlogs:\n")
		for _, line := range d.logs {
			fmt.Fprintf(&b, "  %s\n", line)
		}
	}
	return b.String()
}

func (s *snapshot) reportFiles(b *strings.Builder) {
	b.WriteString("\n## Files in this snapshot\n\n")
	names := make([]string, 0, len(s.files))
	for _, f := range s.files {
		names = append(names, f.name)
	}
	sort.Strings(names)
	for _, n := range names {
		fmt.Fprintf(b, "- `%s`\n", n)
	}
}

// daemonStateWord collapses the four booleans into the word a reader wants.
// The order matters: "not installed" explains "not loaded", which explains
// "not running", and reporting the innermost symptom hides the cause.
func daemonStateWord(st daemonStatus) string {
	switch {
	case st.Err != nil:
		return "not configured"
	case !st.Installed:
		return "not installed"
	case !st.Loaded:
		return "not loaded"
	case !st.Running:
		return "loaded, not running"
	default:
		return "running"
	}
}

func pidOrDash(st daemonStatus) string {
	if st.PID <= 0 {
		return "-"
	}
	return strconv.Itoa(st.PID)
}

func orNone(s string) string {
	if strings.TrimSpace(s) == "" {
		return "(none)"
	}
	return s
}

// snapshotCommand is the manual entry point. A failed switch takes a snapshot
// on its own, so this is for the other half of the problem: the machine is
// misbehaving and no switch just failed, or the operator wants a "before"
// to compare a later failure against.
func snapshotCommand(cfgPath, reason string) error {
	cfg, err := loadConfig(cfgPath)
	if err != nil {
		return err
	}
	if strings.TrimSpace(reason) == "" {
		reason = "taken by hand"
	}
	fmt.Println("collecting; this reads the system only and changes nothing...")
	dir, err := writeSnapshot(cfgPath, cfg, snapshotRequest{Reason: reason})
	if err != nil {
		return err
	}
	fmt.Printf("snapshot: %s\n", dir)
	fmt.Printf("start with %s\n", filepath.Join(dir, "report.md"))
	fmt.Printf("to share it: tar czf ~/Desktop/macswitcher-snapshot.tgz -C %s %s\n",
		filepath.Dir(dir), filepath.Base(dir))
	return nil
}
