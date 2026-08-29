package app

import (
	"bufio"
	"io"
	"os"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
)

const (
	// tailBytes bounds how much of a log file the modal reads. Reading only
	// the tail keeps opening the modal cheap however large a log has grown
	// between newsyslog rotations.
	tailBytes = 256 << 10
	// tailLines bounds how many lines the modal keeps out of that read, so a
	// log of very short lines cannot blow up the viewport's content either.
	tailLines = 2000
	// followInterval is how often a modal in follow mode re-reads its log.
	followInterval = time.Second
)

// logKind distinguishes the two files a launchd job leaves behind under the
// conventions in internal/service: the log the process writes itself, and
// launchd's capture of its stderr, which normally holds panics only.
type logKind int

const (
	logKindMain logKind = iota
	logKindErr
)

func (k logKind) String() string {
	if k == logKindErr {
		return "stderr"
	}
	return "log"
}

// logSource is one readable log file belonging to a daemon row.
type logSource struct {
	kind logKind
	path string
}

// daemonLogSources resolves the log files for row, main log first.
//
// macswitcher's own agent knows its paths from internal/service. For every
// other daemon the plist is the only source of truth, so StandardOutPath and
// StandardErrorPath are read out of it. Under the shared conventions a job
// declares only StandardErrorPath (~/Library/Logs/<name>.err.log) and writes
// ~/Library/Logs/<name>.log itself, so the main log is derived from the
// stderr path whenever the plist does not name it. A job that logs to
// StandardIO instead — Homebrew's unbound does — yields no sources at all.
func daemonLogSources(row daemonRow) []logSource {
	if row.kind == daemonKindAlpaca {
		svc := launchAgentService()
		var sources []logSource
		if path, err := svc.LogPath(); err == nil {
			sources = append(sources, logSource{kind: logKindMain, path: path})
		}
		if path, err := svc.ErrLogPath(); err == nil {
			sources = append(sources, logSource{kind: logKindErr, path: path})
		}
		return sources
	}
	if strings.TrimSpace(row.label) == "" {
		return nil
	}
	plistPath, err := agentPlistPath(row.scope, row.label)
	if err != nil {
		return nil
	}
	mainPath := plistString(plistPath, "StandardOutPath")
	errPath := plistString(plistPath, "StandardErrorPath")
	if mainPath == "" {
		mainPath = deriveMainLogPath(errPath)
	}
	var sources []logSource
	if mainPath != "" {
		sources = append(sources, logSource{kind: logKindMain, path: mainPath})
	}
	if errPath != "" && errPath != mainPath {
		sources = append(sources, logSource{kind: logKindErr, path: errPath})
	}
	return sources
}

// deriveMainLogPath maps ~/Library/Logs/<name>.err.log to its sibling
// ~/Library/Logs/<name>.log. Anything not following that convention yields
// "" rather than a guess.
func deriveMainLogPath(errPath string) string {
	const suffix = ".err.log"
	if !strings.HasSuffix(errPath, suffix) {
		return ""
	}
	return strings.TrimSuffix(errPath, suffix) + ".log"
}

// plistString reads one string-valued key out of a plist. plutil does the
// parsing rather than an XML reader here because plists under
// /Library/LaunchDaemons are as often binary as they are XML. A missing key
// is not an error: it means the job does not redirect that stream.
func plistString(plistPath, key string) string {
	out, err := runCommandOutput("plutil", "-extract", key, "raw", "-o", "-", plistPath)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(out)
}

// readLogTail returns the last lines of path, oldest first.
func readLogTail(path string) ([]string, error) {
	f, err := os.Open(path) // #nosec G304 -- path comes from a launchd plist or internal/service, not from user input
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()

	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	var offset int64
	if info.Size() > tailBytes {
		offset = info.Size() - tailBytes
	}
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return nil, err
	}

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64<<10), 1<<20)
	lines := make([]string, 0, 256)
	for scanner.Scan() {
		lines = append(lines, scanner.Text())
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	// Seeking into the middle of the file lands mid-line, and that first
	// fragment would render as a truncated entry, so drop it.
	if offset > 0 && len(lines) > 0 {
		lines = lines[1:]
	}
	if len(lines) > tailLines {
		lines = lines[len(lines)-tailLines:]
	}
	return lines, nil
}

type logLoadedMsg struct {
	path  string
	lines []string
	err   error
}

func loadLogCmd(src logSource) tea.Cmd {
	return func() tea.Msg {
		lines, err := readLogTail(src.path)
		return logLoadedMsg{path: src.path, lines: lines, err: err}
	}
}

// logFollowMsg drives one re-read in follow mode. seq identifies the follow
// session it belongs to: toggling follow off and on again would otherwise
// leave the older tick chain running alongside the new one, doubling the
// re-read rate for as long as the modal stays open.
type logFollowMsg struct{ seq int }

func followCmd(seq int) tea.Cmd {
	return tea.Tick(followInterval, func(time.Time) tea.Msg { return logFollowMsg{seq: seq} })
}

// logModal is the log viewer shown in place of the daemon list: a scrollable
// snapshot of one daemon's log, re-read on a timer while follow is on.
type logModal struct {
	open     bool
	rowName  string
	sources  []logSource
	active   int
	viewport viewport.Model
	follow   bool
	seq      int
	loadErr  error
	// note is shown instead of the viewport when there is no log to read at
	// all, e.g. a daemon whose plist redirects nothing.
	note string
}

// newLogViewport builds the modal's viewport with the two default bindings
// that collide with the modal's own keys removed: "f" (page down) is follow
// here, and "l" (scroll right) closes the modal.
func newLogViewport(width, height int) viewport.Model {
	vp := viewport.New(width, height)
	vp.KeyMap.PageDown = key.NewBinding(key.WithKeys("pgdown", " "), key.WithHelp("pgdn", "page down"))
	vp.KeyMap.Right = key.NewBinding(key.WithKeys("right"), key.WithHelp("→", "move right"))
	return vp
}

// source returns the log file the modal is currently showing.
func (l logModal) source() (logSource, bool) {
	if l.active < 0 || l.active >= len(l.sources) {
		return logSource{}, false
	}
	return l.sources[l.active], true
}

func (l *logModal) setSize(width, height int) {
	l.viewport.Width = width
	l.viewport.Height = height
}

// setContent replaces the viewport's lines, holding the reader's position:
// following (or already parked at the bottom) jumps to the newest line, and
// anything else keeps the scroll offset so a background re-read does not
// yank the view out from under someone reading further up.
func (l *logModal) setContent(lines []string) {
	atBottom := l.viewport.AtBottom()
	offset := l.viewport.YOffset
	if len(lines) == 0 {
		l.viewport.SetContent(styleDim.Render("(empty)"))
	} else {
		l.viewport.SetContent(strings.Join(lines, "\n"))
	}
	if l.follow || atBottom {
		l.viewport.GotoBottom()
		return
	}
	l.viewport.SetYOffset(offset)
}
