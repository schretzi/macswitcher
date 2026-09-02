package app

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"gopkg.in/yaml.v3"
)

// daemonKind selects which extra, daemon-specific detail lines observeModel
// gathers for a row, beyond the generic launchd state.
type daemonKind int

const (
	daemonKindAlpaca daemonKind = iota
	daemonKindAdGuard
	daemonKindContainer
	daemonKindKerberos
	daemonKindOMT
	daemonKindVPN
	daemonKindTunneling
	daemonKindPrivoxy
	daemonKindLsrules
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
	width        int
	height       int
	message      string
	messageIsErr bool
	quitting     bool
	logs         logModal
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
		{name: appAdGuard, configKey: appAdGuard, label: cfg.Daemons.AdGuardHome.Label, scope: cfg.Daemons.AdGuardHome.Scope, kind: daemonKindAdGuard},
		// apple/container + kiac: the cluster VMs take their DNS from the vmnet
		// gateway, so this belongs next to the resolvers rather than at the end.
		{name: appContainer, configKey: appContainer, label: cfg.Daemons.Container.Label, scope: cfg.Daemons.Container.Scope, kind: daemonKindContainer},
		// directly after alpaca: it is the hop alpaca forwards to in direct
		// contexts, and the pair is only meaningful read together.
		{name: appPrivoxy, configKey: appPrivoxy, label: cfg.Daemons.Privoxy.Label, scope: cfg.Daemons.Privoxy.Scope, kind: daemonKindPrivoxy},
		{name: "kerberoskeepalive", configKey: "kerberos_keep_alive", label: cfg.Daemons.KerberosKeepAlive.Label, scope: cfg.Daemons.KerberosKeepAlive.Scope, kind: daemonKindKerberos},
		{name: "omt", configKey: "omt", label: cfg.Daemons.OMT.Label, scope: cfg.Daemons.OMT.Scope, kind: daemonKindOMT},
		{name: appVPN, configKey: appVPN, label: cfg.Daemons.VPN.Label, scope: cfg.Daemons.VPN.Scope, kind: daemonKindVPN},
		// last: its gcp tunnels ride on whatever the rows above have set up
		// (proxy, resolver, VPN), so a failure here is usually a symptom of
		// one of them.
		// next to tunneling at the end: both are services this machine runs
		// for itself rather than parts of the network path.
		{name: appLsrules, configKey: appLsrules, label: cfg.Daemons.Lsrules.Label, scope: cfg.Daemons.Lsrules.Scope, kind: daemonKindLsrules},
		{name: "tunneling", configKey: "tunneling", label: cfg.Daemons.Tunneling.Label, scope: cfg.Daemons.Tunneling.Scope, kind: daemonKindTunneling},
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
// Kerberos ticket validity, omt's OAuth2 token status, the VPN tunnel
// interface's connectivity, or how many of tunneling's local ports are open.
func gatherExtra(cfg Config, row daemonRow) []string {
	switch row.kind {
	case daemonKindAlpaca:
		return alpacaDetail(cfg)
	case daemonKindPrivoxy:
		return privoxyDetail(cfg)
	case daemonKindLsrules:
		return lsrulesDetail()
	case daemonKindAdGuard:
		return adguardDetail(cfg)
	case daemonKindContainer:
		return containerRuntimeDetail()
	case daemonKindKerberos:
		return kerberosDetail(cfg)
	case daemonKindOMT:
		return omtDetail()
	case daemonKindVPN:
		return vpnDetail(cfg)
	case daemonKindTunneling:
		return tunnelingDetail()
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

// adguardDetail shows what AdGuard Home is actually forwarding to, split into
// the default upstreams macswitcher owns and the per-domain ones generated
// and the per-domain ones an overlay contributes - the distinction that
// matters when the intranet stops resolving after a context switch.
func adguardDetail(cfg Config) []string {
	path := strings.TrimSpace(cfg.AdGuard.UpstreamsFile)
	if path == "" {
		return []string{"not configured (set adguard.upstreams_file to enable)"}
	}
	defaults, specific, err := currentAdGuardUpstreams(path)
	if err != nil {
		return []string{"cannot read " + path + ": " + firstLine(err.Error())}
	}
	if len(defaults) == 0 && len(specific) == 0 {
		return []string{"no upstreams in " + path}
	}
	lines := []string{}
	if len(defaults) > 0 {
		lines = append(lines, "upstreams: "+strings.Join(defaults, ", "))
	} else {
		lines = append(lines, "no default upstreams - AdGuard Home cannot resolve anything")
	}
	if len(specific) > 0 {
		lines = append(lines, fmt.Sprintf("per-domain: %d zone(s)", len(specific)))
	}
	return lines
}

// kerberosCcacheFromKeepAlive reads KerberosKeepAlive's own config and returns
// the ccache path of its first profile.
//
// The ticket and the proxy credential are separate things that only sometimes
// coincide. A context authenticates to its forward proxy either with a ticket
// or with a Keychain password, so forwarder_proxy.ticket_file is empty
// whenever the proxy is password-based - but KerberosKeepAlive is still
// maintaining a ticket, because plenty of other things on a corporate network
// need one. Reading its config finds the ticket in that case instead of
// reporting "not configured" while a perfectly valid ticket sits on disk.
func kerberosCcacheFromKeepAlive() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	data, err := os.ReadFile(filepath.Join(home, ".config", "kerberoskeepalive", "config.yaml"))
	if err != nil {
		return ""
	}
	var parsed struct {
		Profiles []struct {
			CcachePath string `yaml:"ccache_path"`
		} `yaml:"profiles"`
	}
	if err := yaml.Unmarshal(data, &parsed); err != nil {
		return ""
	}
	for _, p := range parsed.Profiles {
		if strings.TrimSpace(p.CcachePath) != "" {
			return strings.TrimSpace(p.CcachePath)
		}
	}
	return ""
}

func kerberosDetail(cfg Config) []string {
	ctx := cfg.Contexts[cfg.CurrentContext]
	ticketFile := ""
	if ctx.ForwarderProxy != nil {
		ticketFile = ctx.ForwarderProxy.TicketFile
	}
	source := "forwarder_proxy.ticket_file"
	if strings.TrimSpace(ticketFile) == "" {
		if fallback := kerberosCcacheFromKeepAlive(); fallback != "" {
			ticketFile = fallback
			source = "kerberoskeepalive ccache_path"
		}
	}
	valid, summary, detail := kerberosTicketStatus(ticketFile)
	lines := []string{fmt.Sprintf("ticket (%s): %s", ticketFile, summary)}
	if ticketFile != "" {
		lines = append(lines, "source: "+source)
	}
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

// lsrulesDetail answers the question the row cannot: Little Snitch keeps the
// rules it already downloaded, so a server that stopped serving is invisible
// from the subscription side. What matters is whether the port still answers
// TLS, and whether the certificate is about to expire - nothing renews it.
func lsrulesDetail() []string {
	if !lsrulesInstalled() {
		return []string{"lsrules binary not found on PATH"}
	}
	st, err := lsrulesServeStatus()
	if err != nil {
		return []string{fmt.Sprintf("lsrules status failed: %v", firstLine(err.Error()))}
	}
	lines := []string{fmt.Sprintf("%s, %d rule group(s)", st.BaseURL, len(st.RuleGroups))}
	switch {
	case !st.Listening:
		lines = append(lines, "not listening - subscriptions cannot refresh")
	case !st.TLSOK:
		lines = append(lines, "listening but TLS fails: "+firstLine(st.TLSError))
	default:
		lines = append(lines, "listening, TLS verified")
	}
	switch {
	case st.CertificateError != "":
		lines = append(lines, "certificate: "+firstLine(st.CertificateError))
	case st.Certificate.ExpiresIn <= 0:
		lines = append(lines, "certificate EXPIRED - reissue with local_ca")
	case st.Certificate.ExpiresIn < 30:
		lines = append(lines, fmt.Sprintf("certificate expires in %d days - reissue with local_ca", st.Certificate.ExpiresIn))
	}
	return lines
}

func tunnelingDetail() []string {
	if !tunnelingInstalled() {
		return []string{"tunneling binary not found on PATH"}
	}
	summary, lines, err := tunnelingTunnelStatus()
	if err != nil {
		return []string{fmt.Sprintf("tunneling status failed: %v", err)}
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
		if m.logs.open {
			return m.handleLogKey(msg)
		}
		return m.handleKey(msg)
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		m.logs.setSize(m.modalViewportSize())
		return m, nil
	case logLoadedMsg:
		return m.handleLogLoaded(msg)
	case logFollowMsg:
		return m.handleLogFollow(msg)
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
	case "l":
		return m.openLogModal()
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

// openLogModal opens the log viewer on the selected row. A row with no
// launchd label has nothing to look at, and one whose plist redirects
// neither stream (Homebrew's unbound logs to StandardIO) opens with a note
// saying so, rather than an empty pane the reader has to interpret.
func (m observeModel) openLogModal() (tea.Model, tea.Cmd) {
	row := m.rows[m.cursor]
	if strings.TrimSpace(row.label) == "" {
		m.message = row.name + ": no launchd label configured, so no log to show"
		m.messageIsErr = true
		return m, nil
	}
	m.logs = logModal{
		open:     true,
		rowName:  row.name,
		sources:  daemonLogSources(row),
		viewport: newLogViewport(m.modalViewportSize()),
	}
	src, ok := m.logs.source()
	if !ok {
		m.logs.note = row.label + " declares no StandardOutPath or StandardErrorPath, so it writes no log file"
		return m, nil
	}
	return m, loadLogCmd(src)
}

// handleLogLoaded installs a finished read, unless it is stale: a load that
// lands after the modal closed, or after tab moved to the other file, would
// otherwise overwrite whatever is on screen now.
func (m observeModel) handleLogLoaded(msg logLoadedMsg) (tea.Model, tea.Cmd) {
	src, ok := m.logs.source()
	if !m.logs.open || !ok || src.path != msg.path {
		return m, nil
	}
	m.logs.loadErr = msg.err
	m.logs.setContent(msg.lines)
	return m, nil
}

// handleLogFollow re-reads the followed log and schedules the next tick, as
// long as this tick still belongs to the live follow session.
func (m observeModel) handleLogFollow(msg logFollowMsg) (tea.Model, tea.Cmd) {
	src, ok := m.logs.source()
	if !m.logs.open || !m.logs.follow || msg.seq != m.logs.seq || !ok {
		return m, nil
	}
	return m, tea.Batch(loadLogCmd(src), followCmd(msg.seq))
}

func (m observeModel) handleLogKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc", "q", "l", "ctrl+c":
		m.logs = logModal{}
		return m, nil
	case "tab", "shift+tab":
		if len(m.logs.sources) < 2 {
			return m, nil
		}
		step := 1
		if msg.String() == "shift+tab" {
			step = len(m.logs.sources) - 1
		}
		m.logs.active = (m.logs.active + step) % len(m.logs.sources)
		m.logs.loadErr = nil
		src, _ := m.logs.source()
		m.logs.setContent(nil)
		return m, loadLogCmd(src)
	case "f":
		m.logs.follow = !m.logs.follow
		src, ok := m.logs.source()
		if !m.logs.follow || !ok {
			return m, nil
		}
		m.logs.seq++
		return m, tea.Batch(loadLogCmd(src), followCmd(m.logs.seq))
	case "r":
		src, ok := m.logs.source()
		if !ok {
			return m, nil
		}
		return m, loadLogCmd(src)
	}
	var cmd tea.Cmd
	m.logs.viewport, cmd = m.logs.viewport.Update(msg)
	// Scrolling back through history is incompatible with being dragged to
	// the newest line every second, so reading away from the bottom turns
	// follow off rather than fighting it.
	if m.logs.follow && !m.logs.viewport.AtBottom() {
		m.logs.follow = false
	}
	return m, cmd
}
