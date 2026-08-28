package app

import (
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// daemonKind selects which extra, daemon-specific detail lines observeModel
// gathers for a row, beyond the generic launchd state.
type daemonKind int

const (
	daemonKindAlpaca daemonKind = iota
	daemonKindUnbound
	daemonKindKerberos
	daemonKindOMT
	daemonKindVPN
)

// daemonRow is one line of the observe TUI: a launchd agent plus the
// daemon-specific detail lines gathered for it.
type daemonRow struct {
	name      string // display name shown in the TUI
	configKey string // dotted key under daemons: in config.yaml, e.g. "kerberos_keep_alive"
	label     string
	scope     string
	kind      daemonKind
	status    daemonStatus
	extra     []string
}

type observeModel struct {
	cfg          Config
	rows         []daemonRow
	cursor       int
	message      string
	messageIsErr bool
	quitting     bool
}

// Observe starts the interactive TUI showing the status of macswitcher's own
// Alpaca agent plus the external daemons configured under `daemons:`
// (unbound, kerberos_keep_alive, omt, vpn), with actions to (re)start, stop,
// enable, and disable each one.
func Observe(cfgPath string) error {
	cfg, err := loadConfig(cfgPath)
	if err != nil {
		return err
	}
	m := newObserveModel(cfg)
	p := tea.NewProgram(m, tea.WithAltScreen())
	_, err = p.Run()
	return err
}

func newObserveModel(cfg Config) observeModel {
	rows := []daemonRow{
		// macswitcher's own job, so its label comes from internal/service
		// rather than from config: it is not something the user can point
		// elsewhere.
		{name: appAlpaca, configKey: "", label: launchAgentService().Label(), scope: daemonScopeUser, kind: daemonKindAlpaca},
		{name: appUnbound, configKey: appUnbound, label: cfg.Daemons.Unbound.Label, scope: cfg.Daemons.Unbound.Scope, kind: daemonKindUnbound},
		{name: "kerberoskeepalive", configKey: "kerberos_keep_alive", label: cfg.Daemons.KerberosKeepAlive.Label, scope: cfg.Daemons.KerberosKeepAlive.Scope, kind: daemonKindKerberos},
		{name: "omt", configKey: "omt", label: cfg.Daemons.OMT.Label, scope: cfg.Daemons.OMT.Scope, kind: daemonKindOMT},
		{name: "vpn", configKey: "vpn", label: cfg.Daemons.VPN.Label, scope: cfg.Daemons.VPN.Scope, kind: daemonKindVPN},
	}
	return observeModel{cfg: cfg, rows: rows}
}

func (m observeModel) Init() tea.Cmd {
	cmds := make([]tea.Cmd, 0, len(m.rows)+1)
	for i := range m.rows {
		cmds = append(cmds, refreshRowCmd(m.cfg, i, m.rows[i]))
	}
	cmds = append(cmds, tickCmd())
	return tea.Batch(cmds...)
}

type refreshMsg struct {
	index  int
	status daemonStatus
	extra  []string
}

type actionResultMsg struct {
	index int
	verb  string
	err   error
}

type tickMsg time.Time

func tickCmd() tea.Cmd {
	return tea.Tick(5*time.Second, func(t time.Time) tea.Msg { return tickMsg(t) })
}

func refreshRowCmd(cfg Config, index int, row daemonRow) tea.Cmd {
	return func() tea.Msg {
		status := inspectDaemon(row.label, row.scope)
		return refreshMsg{index: index, status: status, extra: gatherExtra(cfg, row)}
	}
}

// gatherExtra collects the daemon-specific detail lines shown under a row:
// alpaca's active proxy wiring, unbound's forwarders, the active context's
// Kerberos ticket validity, omt's OAuth2 token status, or the VPN tunnel
// interface's connectivity.
func gatherExtra(cfg Config, row daemonRow) []string {
	switch row.kind {
	case daemonKindAlpaca:
		return alpacaDetail(cfg)
	case daemonKindUnbound:
		return unboundDetail(cfg)
	case daemonKindKerberos:
		return kerberosDetail(cfg)
	case daemonKindOMT:
		return omtDetail()
	case daemonKindVPN:
		return vpnDetail(cfg)
	default:
		return nil
	}
}

func alpacaDetail(cfg Config) []string {
	if !cfg.Alpaca.Enabled {
		return []string{"alpaca is disabled globally (alpaca.enabled: false)"}
	}
	ctx, ok := cfg.Contexts[cfg.CurrentContext]
	if !ok {
		return []string{fmt.Sprintf("current context %q not found", cfg.CurrentContext)}
	}
	if ctx.Alpaca != nil && !ctx.Alpaca.Enabled {
		return []string{fmt.Sprintf("alpaca is disabled for context %q", cfg.CurrentContext)}
	}
	lines := []string{fmt.Sprintf("context %q, proxy_mode=%s", cfg.CurrentContext, ctx.ProxyMode)}
	switch {
	case isForwardProxyMode(ctx.ProxyMode) && ctx.ForwarderProxy != nil:
		fp := ctx.ForwarderProxy
		auth := "keychain (NTLM/Basic)"
		if strings.TrimSpace(fp.TicketFile) != "" {
			auth = "kerberos ticket_file=" + fp.TicketFile
		}
		lines = append(lines, fmt.Sprintf("forwarding -> %s:%d via %s", fp.ProxyServer, fp.Port, auth))
	case isForwardProxyMode(ctx.ProxyMode):
		lines = append(lines, "proxy_mode is forward but forwarder_proxy is not configured")
	default:
		lines = append(lines, "no upstream forwarding (direct or off)")
	}
	return lines
}

func unboundDetail(cfg Config) []string {
	forwarders := currentUnboundForwarders(cfg.Unbound.ForwardersFile)
	if len(forwarders) == 0 {
		return []string{"no forward-addr entries in " + cfg.Unbound.ForwardersFile}
	}
	return []string{"forwarders: " + strings.Join(forwarders, ", ")}
}

func kerberosDetail(cfg Config) []string {
	ctx := cfg.Contexts[cfg.CurrentContext]
	ticketFile := ""
	if ctx.ForwarderProxy != nil {
		ticketFile = ctx.ForwarderProxy.TicketFile
	}
	valid, summary, detail := kerberosTicketStatus(ticketFile)
	lines := []string{fmt.Sprintf("ticket (%s): %s", ticketFile, summary)}
	if !valid && strings.TrimSpace(detail) != "" {
		lines = append(lines, strings.TrimSpace(detail))
	}
	return lines
}

func omtDetail() []string {
	if !omtInstalled() {
		return []string{"omt binary not found on PATH"}
	}
	summary, lines, err := omtAccountStatus()
	if err != nil {
		return []string{fmt.Sprintf("omt status failed: %v", err)}
	}
	out := []string{summary}
	out = append(out, lines...)
	return out
}

func vpnDetail(cfg Config) []string {
	iface := strings.TrimSpace(cfg.Daemons.VPN.Interface)
	if iface == "" {
		return []string{"no tunnel interface configured (set daemons.vpn.interface to check connectivity)"}
	}
	_, detail := vpnInterfaceStatus(iface)
	return []string{detail}
}

func (m observeModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		return m.handleKey(msg)
	case refreshMsg:
		if msg.index >= 0 && msg.index < len(m.rows) {
			m.rows[msg.index].status = msg.status
			m.rows[msg.index].extra = msg.extra
		}
		return m, nil
	case actionResultMsg:
		row := m.rows[msg.index]
		if msg.err != nil {
			m.message = fmt.Sprintf("%s %s failed: %v", row.name, msg.verb, msg.err)
			m.messageIsErr = true
			return m, nil
		}
		m.message = fmt.Sprintf("%s: %s ok", row.name, msg.verb)
		m.messageIsErr = false
		return m, refreshRowCmd(m.cfg, msg.index, row)
	case tickMsg:
		cmds := make([]tea.Cmd, 0, len(m.rows)+1)
		for i := range m.rows {
			cmds = append(cmds, refreshRowCmd(m.cfg, i, m.rows[i]))
		}
		cmds = append(cmds, tickCmd())
		return m, tea.Batch(cmds...)
	}
	return m, nil
}

func (m observeModel) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "q", "ctrl+c", "esc":
		m.quitting = true
		return m, tea.Quit
	case "up", "k":
		if m.cursor > 0 {
			m.cursor--
		}
		return m, nil
	case "down", "j":
		if m.cursor < len(m.rows)-1 {
			m.cursor++
		}
		return m, nil
	case "r":
		return m, m.refreshAllCmd()
	case "s":
		return m, m.actionCmd(actionStart)
	case "S":
		return m, m.actionCmd(actionStop)
	case "R":
		return m, m.actionCmd(actionRestart)
	case "e":
		return m, m.actionCmd("enable")
	case "d":
		return m, m.actionCmd("disable")
	}
	return m, nil
}

func (m observeModel) refreshAllCmd() tea.Cmd {
	cmds := make([]tea.Cmd, 0, len(m.rows))
	for i := range m.rows {
		cmds = append(cmds, refreshRowCmd(m.cfg, i, m.rows[i]))
	}
	return tea.Batch(cmds...)
}

// actionCmd dispatches verb (start/stop/restart/enable/disable) against the
// currently selected row. User-scoped agents run unprivileged and silently;
// system-scoped daemons need sudo, so the command runs interactively via
// tea.ExecProcess, which hands the real terminal to it (letting sudo prompt
// for a password) and suspends the TUI for the duration.
func (m observeModel) actionCmd(verb string) tea.Cmd {
	index := m.cursor
	row := m.rows[index]
	if strings.TrimSpace(row.label) == "" {
		err := fmt.Errorf("no launchd label configured for %s", row.name)
		return func() tea.Msg { return actionResultMsg{index: index, verb: verb, err: err} }
	}
	if normalizeDaemonScope(row.scope) == daemonScopeSystem {
		cmd, err := systemDaemonActionCmd(verb, row.label)
		if err != nil {
			return func() tea.Msg { return actionResultMsg{index: index, verb: verb, err: err} }
		}
		return tea.ExecProcess(cmd, func(err error) tea.Msg {
			return actionResultMsg{index: index, verb: verb, err: err}
		})
	}
	var action func(label, scope string) error
	switch verb {
	case actionStart:
		action = launchdStart
	case actionStop:
		action = launchdStop
	case actionRestart:
		action = launchdRestart
	case "enable":
		action = launchdEnable
	case "disable":
		action = launchdDisable
	default:
		err := fmt.Errorf("unknown action %q", verb)
		return func() tea.Msg { return actionResultMsg{index: index, verb: verb, err: err} }
	}
	return func() tea.Msg {
		return actionResultMsg{index: index, verb: verb, err: action(row.label, row.scope)}
	}
}

var (
	styleSelected = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("212"))
	styleDim      = lipgloss.NewStyle().Foreground(lipgloss.Color("240"))
	styleRunning  = lipgloss.NewStyle().Foreground(lipgloss.Color("42"))
	styleStopped  = lipgloss.NewStyle().Foreground(lipgloss.Color("227"))
	styleMissing  = lipgloss.NewStyle().Foreground(lipgloss.Color("203"))
	styleError    = lipgloss.NewStyle().Foreground(lipgloss.Color("203"))
	styleHeader   = lipgloss.NewStyle().Bold(true).Underline(true)
)

func (m observeModel) View() string {
	if m.quitting {
		return ""
	}
	var b strings.Builder
	b.WriteString(styleHeader.Render("macswitcher observe") + "\n\n")
	for i, row := range m.rows {
		cursor := "  "
		if i == m.cursor {
			cursor = "> "
		}
		line := cursor + renderRowSummary(row)
		if i == m.cursor {
			line = styleSelected.Render(line)
		}
		b.WriteString(line + "\n")
		for _, extra := range row.extra {
			b.WriteString(styleDim.Render("      "+extra) + "\n")
		}
		b.WriteString("\n")
	}
	if m.message != "" {
		if m.messageIsErr {
			b.WriteString(styleError.Render(m.message) + "\n\n")
		} else {
			b.WriteString(m.message + "\n\n")
		}
	}
	b.WriteString(styleDim.Render(
		"↑/↓ select  s start  S stop  R restart  e enable  d disable  r refresh  q quit",
	))
	return b.String()
}

func renderRowSummary(row daemonRow) string {
	name := fmt.Sprintf("%-18s", row.name)
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
	return name + scopeTag + fmt.Sprintf("%-8s ", row.label) + state
}
