package app

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
)

var (
	styleSelected = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("212"))
	styleDim      = lipgloss.NewStyle().Foreground(lipgloss.Color("240"))
	styleRunning  = lipgloss.NewStyle().Foreground(lipgloss.Color("42"))
	styleStopped  = lipgloss.NewStyle().Foreground(lipgloss.Color("227"))
	styleMissing  = lipgloss.NewStyle().Foreground(lipgloss.Color("203"))
	styleError    = lipgloss.NewStyle().Foreground(lipgloss.Color("203"))
	styleHeader   = lipgloss.NewStyle().Bold(true)
	styleBox      = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(lipgloss.Color("240")).
			Padding(0, 1)
	styleTabActive   = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("212"))
	styleFollowOn    = lipgloss.NewStyle().Foreground(lipgloss.Color("42"))
	listKeyHints     = "↑/↓ select  l logs  s start  S stop  R restart  e enable  d disable  r refresh  q quit"
	logKeyHintFormat = "↑/↓ scroll  pgup/pgdn page  tab switch  f follow (%s)  r reload  esc close"
)

const (
	// defaultTermWidth and defaultTermHeight stand in until the first
	// tea.WindowSizeMsg arrives, which is after the very first render.
	defaultTermWidth  = 100
	defaultTermHeight = 30
	// boxHorizontalChrome is what styleBox costs around its text: one border
	// cell plus one padding cell on each side.
	boxHorizontalChrome = 4
	// boxPadding is the horizontal padding alone. lipgloss's Width() counts
	// padding but not the border, so a box whose text must be n columns wide
	// is given Width(n + boxPadding).
	boxPadding    = 2
	minInnerWidth = 24
	// maxModalWidth keeps log lines readable on a very wide terminal rather
	// than stretching the modal edge to edge.
	maxModalWidth = 160
	minModalWidth = 40
	// modalChromeLines is every row of the modal that is not log text: the
	// two border rows, the title, the tab row, the rule under it and the key
	// hints, plus one spare so the centred box never has to fill the
	// terminal's last line exactly.
	modalChromeLines  = 7
	minViewportHeight = 3
)

// followOff is the log viewer's follow toggle, not a proxy state - it only
// happens to share the word with ProxyModeOff.
const followOff = "off"

func (m observeModel) View() string {
	if m.quitting {
		return ""
	}
	if m.logs.open {
		return m.logModalView()
	}
	return m.listView()
}

// termWidth and termHeight report the terminal size, falling back to sane
// defaults for the first frame, which bubbletea renders before it delivers
// the initial tea.WindowSizeMsg.
func (m observeModel) termWidth() int {
	if m.width > 0 {
		return m.width
	}
	return defaultTermWidth
}

func (m observeModel) termHeight() int {
	if m.height > 0 {
		return m.height
	}
	return defaultTermHeight
}

// listInnerWidth is the width available for content inside the bordered box.
func (m observeModel) listInnerWidth() int {
	return max(m.termWidth()-boxHorizontalChrome, minInnerWidth)
}

// columnWidths sizes the name and label columns from the rows themselves, so
// they line up whatever labels the operator has configured.
func (m observeModel) columnWidths() (name, label int) {
	for _, row := range m.rows {
		name = max(name, lipgloss.Width(row.name))
		label = max(label, lipgloss.Width(row.label))
	}
	return name, label
}

func (m observeModel) listView() string {
	inner := m.listInnerWidth()
	nameWidth, labelWidth := m.columnWidths()

	var b strings.Builder
	b.WriteString(clip(styleHeader.Render("macswitcher observe"), inner) + "\n")
	b.WriteString(styleDim.Render(strings.Repeat("═", inner)) + "\n")
	for i, row := range m.rows {
		if i > 0 {
			b.WriteString(styleDim.Render(strings.Repeat("─", inner)) + "\n")
		}
		cursor := "  "
		if i == m.cursor {
			cursor = "> "
		}
		line := cursor + renderRowSummary(row, nameWidth, labelWidth)
		if i == m.cursor {
			line = styleSelected.Render(line)
		}
		b.WriteString(clip(line, inner) + "\n")
		for _, extra := range row.extra {
			b.WriteString(clip(styleDim.Render("    "+extra), inner) + "\n")
		}
	}

	out := styleBox.Width(inner + boxPadding).Render(strings.TrimRight(b.String(), "\n"))
	if m.message != "" {
		message := m.message
		if m.messageIsErr {
			message = styleError.Render(message)
		}
		out += "\n" + clip(message, m.termWidth())
	}
	return out + "\n" + clip(styleDim.Render(listKeyHints), m.termWidth())
}

func renderRowSummary(row daemonRow, nameWidth, labelWidth int) string {
	name := pad(row.name, nameWidth) + "  "
	if strings.TrimSpace(row.label) == "" {
		return name + styleMissing.Render(fmt.Sprintf("not configured (set daemons.%s.label)", row.configKey))
	}
	scopeTag := ""
	if normalizeDaemonScope(row.scope) == daemonScopeSystem {
		scopeTag = "[system] "
	}
	if row.status.Err != nil {
		return name + styleMissing.Render(scopeTag+row.status.Err.Error())
	}
	if !row.status.Installed {
		return name + styleMissing.Render(fmt.Sprintf("%snot installed (%s)", scopeTag, row.label))
	}
	var state string
	switch {
	case row.status.Running:
		uptime := "unknown uptime"
		if !row.status.StartedAt.IsZero() {
			uptime = "up " + time.Since(row.status.StartedAt).Round(time.Second).String()
		}
		state = styleRunning.Render(fmt.Sprintf("running pid=%d %s runs=%d", row.status.PID, uptime, row.status.Runs))
	case row.status.Loaded:
		state = styleStopped.Render(fmt.Sprintf("loaded, not running (last exit=%d)", row.status.LastExitCode))
	default:
		state = styleDim.Render("stopped")
	}
	return name + scopeTag + pad(row.label, labelWidth) + "  " + state
}

// modalViewportSize is the width and height available to the log viewport
// inside the modal box.
func (m observeModel) modalViewportSize() (width, height int) {
	width = min(max(m.termWidth()-boxHorizontalChrome, minModalWidth), maxModalWidth)
	height = m.termHeight() - modalChromeLines
	return width, max(height, minViewportHeight)
}

func (m observeModel) logModalView() string {
	width, _ := m.modalViewportSize()

	title := styleHeader.Render(m.logs.rowName + " logs")
	var b strings.Builder
	b.WriteString(clip(title, width) + "\n")
	b.WriteString(clip(m.renderLogTabs(), width) + "\n")
	b.WriteString(styleDim.Render(strings.Repeat("─", width)) + "\n")

	switch {
	case m.logs.note != "":
		b.WriteString(clip(styleMissing.Render(m.logs.note), width))
	case m.logs.loadErr != nil:
		b.WriteString(clip(styleMissing.Render(m.logs.loadErr.Error()), width))
	default:
		b.WriteString(m.logs.viewport.View())
	}

	followState := followOff
	if m.logs.follow {
		followState = styleFollowOn.Render("on")
	}
	b.WriteString("\n" + clip(styleDim.Render(fmt.Sprintf(logKeyHintFormat, followState)), width))

	box := styleBox.Width(width + boxPadding).Render(b.String())
	return lipgloss.Place(m.termWidth(), m.termHeight(), lipgloss.Center, lipgloss.Center, box)
}

// renderLogTabs shows which of the daemon's log files is on screen, and the
// path it resolved to, so an empty view is distinguishable from a wrong file.
func (m observeModel) renderLogTabs() string {
	if len(m.logs.sources) == 0 {
		return styleDim.Render("no log file")
	}
	tabs := make([]string, 0, len(m.logs.sources))
	for i, src := range m.logs.sources {
		if i == m.logs.active {
			tabs = append(tabs, styleTabActive.Render("["+src.kind.String()+"]"))
			continue
		}
		tabs = append(tabs, styleDim.Render(" "+src.kind.String()+" "))
	}
	line := strings.Join(tabs, " ")
	if src, ok := m.logs.source(); ok {
		line += "  " + styleDim.Render(src.path)
	}
	return line
}

// clip trims a rendered line to width columns. lipgloss does the trimming so
// that a cut lands between cells rather than inside an escape sequence.
func clip(s string, width int) string {
	return lipgloss.NewStyle().MaxWidth(width).Render(s)
}

// pad right-pads s to width display columns, leaving anything already wider
// untouched: a column is a floor, not a ceiling, and clip enforces the frame.
func pad(s string, width int) string {
	if gap := width - lipgloss.Width(s); gap > 0 {
		return s + strings.Repeat(" ", gap)
	}
	return s
}
