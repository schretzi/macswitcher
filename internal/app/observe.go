package app

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
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
	// daemonKindGeneric is any daemons: entry with no built-in status
	// function - the "no dedicated code" path for a new daemon like kanata:
	// plain launchd state plus, if configured, StatusCommand's output.
	daemonKindGeneric
)

// daemonRow is one line of the observe TUI: a launchd agent plus the
// daemon-specific detail lines gathered for it.
type daemonRow struct {
	name      string // display name shown in the TUI
	configKey string // dotted key under daemons: in config.yaml, e.g. "kerberos_keep_alive"
	label     string
	scope     string
	kind      daemonKind
	// cfgDaemon is the full config entry, needed by the generic path
	// (StatusCommand for detail, Start/Stop/RestartCommand for actions) and
	// unused by the built-in-kind daemons above, which read cfg directly.
	cfgDaemon DaemonConfig
	status    daemonStatus
	extra     []string
}

type observeModel struct {
	cfg Config
	// cfgPath is kept so the switch modal can pass --config to the child
	// process and reload the config after a switch rewrote current_context.
	cfgPath      string
	rows         []daemonRow
	cursor       int
	width        int
	height       int
	message      string
	messageIsErr bool
	quitting     bool
	logs         logModal
	switcher     switchModal
	// pending is a destructive action on a connectivity-critical daemon that
	// is waiting for a y/n answer. nil when nothing is being confirmed.
	pending *pendingAction
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
	m := newObserveModel(cfg, cfgPath)
	p := tea.NewProgram(m, tea.WithAltScreen())
	_, err = p.Run()
	return err
}

func newObserveModel(cfg Config, cfgPath string) observeModel {
	rows := []daemonRow{
		// macswitcher's own job, so its label comes from internal/service
		// rather than from config: it is not something the user can point
		// elsewhere.
		{name: appAlpaca, configKey: "", label: launchAgentService().Label(), scope: daemonScopeUser, kind: daemonKindAlpaca},
		{name: appAdGuard, configKey: appAdGuard, label: cfg.Daemons[appAdGuard].Label, scope: cfg.Daemons[appAdGuard].Scope, kind: daemonKindAdGuard, cfgDaemon: cfg.Daemons[appAdGuard]},
		// apple/container + kiac: the cluster VMs take their DNS from the vmnet
		// gateway, so this belongs next to the resolvers rather than at the end.
		{name: appContainer, configKey: appContainer, label: cfg.Daemons[appContainer].Label, scope: cfg.Daemons[appContainer].Scope, kind: daemonKindContainer, cfgDaemon: cfg.Daemons[appContainer]},
		// directly after alpaca: it is the hop alpaca forwards to in direct
		// contexts, and the pair is only meaningful read together.
		{name: appPrivoxy, configKey: appPrivoxy, label: cfg.Daemons[appPrivoxy].Label, scope: cfg.Daemons[appPrivoxy].Scope, kind: daemonKindPrivoxy, cfgDaemon: cfg.Daemons[appPrivoxy]},
		{name: "kerberoskeepalive", configKey: "kerberos_keep_alive", label: cfg.Daemons["kerberos_keep_alive"].Label, scope: cfg.Daemons["kerberos_keep_alive"].Scope, kind: daemonKindKerberos, cfgDaemon: cfg.Daemons["kerberos_keep_alive"]},
		{name: "omt", configKey: "omt", label: cfg.Daemons["omt"].Label, scope: cfg.Daemons["omt"].Scope, kind: daemonKindOMT, cfgDaemon: cfg.Daemons["omt"]},
		{name: appVPN, configKey: appVPN, label: cfg.Daemons[appVPN].Label, scope: cfg.Daemons[appVPN].Scope, kind: daemonKindVPN, cfgDaemon: cfg.Daemons[appVPN]},
		// last: its gcp tunnels ride on whatever the rows above have set up
		// (proxy, resolver, VPN), so a failure here is usually a symptom of
		// one of them.
		// next to tunneling at the end: both are services this machine runs
		// for itself rather than parts of the network path.
		{name: appLsrules, configKey: appLsrules, label: cfg.Daemons[appLsrules].Label, scope: cfg.Daemons[appLsrules].Scope, kind: daemonKindLsrules, cfgDaemon: cfg.Daemons[appLsrules]},
		{name: "tunneling", configKey: "tunneling", label: cfg.Daemons["tunneling"].Label, scope: cfg.Daemons["tunneling"].Scope, kind: daemonKindTunneling, cfgDaemon: cfg.Daemons["tunneling"]},
	}
	rows = append(rows, genericDaemonRows(cfg)...)
	return observeModel{cfg: cfg, cfgPath: cfgPath, rows: rows}
}

// genericDaemonRows turns any daemons: entry macswitcher has no built-in
// daemonKind for (e.g. "kanata", added purely via config with no Go code
// change) into rows, in alphabetical order after the built-in ones above.
func genericDaemonRows(cfg Config) []daemonRow {
	extraKeys := make([]string, 0, len(cfg.Daemons))
	for key := range cfg.Daemons {
		if !knownDaemonKeys[key] {
			extraKeys = append(extraKeys, key)
		}
	}
	sort.Strings(extraKeys)
	rows := make([]daemonRow, 0, len(extraKeys))
	for _, key := range extraKeys {
		daemonCfg := cfg.Daemons[key]
		rows = append(rows, daemonRow{
			name: key, configKey: key,
			label: daemonCfg.Label, scope: daemonCfg.Scope,
			kind: daemonKindGeneric, cfgDaemon: daemonCfg,
		})
	}
	return rows
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
	case daemonKindGeneric:
		return genericDetail(row.cfgDaemon)
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
		// alpaca decides per request: Negotiate if a ticket is available,
		// Basic from the Keychain password otherwise.
		lines = append(lines, fmt.Sprintf("forwarding -> %s:%d via negotiate-then-basic", fp.ProxyServer, fp.Port))
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
	// Filtering is only worth a line when it is off: that is the surprising
	// state, and one that a context switch can cause without the operator
	// having asked for it in this session.
	if enabled, err := adguardProtectionEnabled(cfg); err == nil && !enabled {
		lines = append(lines, "filtering: OFF")
	}
	return lines
}

// kerberosCcacheFromKeepAlive reads KerberosKeepAlive's own config and returns
// the ccache path of its first profile.
//
// This is the only place the ticket is named. macswitcher used to carry its
// own forwarder_proxy.ticket_file, which conflated two separate things: where
// the ticket lives, and whether the proxy authenticates with it.
// KerberosKeepAlive, which creates the file, is the authority on its location;
// proxyEnv turns this path into KRB5CCNAME for alpaca's GSS integration.
func kerberosCcacheFromKeepAlive() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	// #nosec G304 -- fixed path under the user's own home dir; KerberosKeepAlive's config location is not caller-controlled
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

func kerberosDetail(_ Config) []string {
	ticketFile := kerberosCcacheFromKeepAlive()
	valid, summary, detail := kerberosTicketStatus(ticketFile)
	lines := []string{fmt.Sprintf("ticket (%s): %s", ticketFile, summary)}
	if !valid {
		// Not an outage on its own: alpaca falls back to Basic against the
		// forward proxy, so say so rather than leaving it looking fatal.
		lines = append(lines, "proxy auth falls back to Basic while this is invalid")
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
	iface := strings.TrimSpace(cfg.Daemons[appVPN].Interface)
	if iface == "" {
		return []string{"no tunnel interface configured (set daemons.vpn.interface to check connectivity)"}
	}
	_, detail := vpnInterfaceStatus(iface)
	return []string{detail}
}

func (m observeModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		if m.switcher.open {
			return m.handleSwitchKey(msg)
		}
		if m.logs.open {
			return m.handleLogKey(msg)
		}
		return m.handleKey(msg)
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		m.logs.setSize(m.modalViewportSize())
		m.switcher.setSize(m.modalViewportSize())
		return m, nil
	case switchEventMsg:
		return m.handleSwitchEvent(msg)
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
			m.message = fmt.Sprintf("%s %s failed: %v", row.name, displayVerb(msg.verb), msg.err)
			m.messageIsErr = true
			return m, nil
		}
		m.message = fmt.Sprintf("%s: %s ok", row.name, displayVerb(msg.verb))
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

// keyInterrupt is ctrl+c. Every modal binds it alongside its own close key,
// because a terminal user reaches for it out of habit and a TUI that ignores
// it looks hung.
const keyInterrupt = "ctrl+c"

// keyEscape is esc, bound as "go back one level" throughout: it closes a
// modal, answers a confirmation with no, and quits from the list.
const keyEscape = "esc"

func (m observeModel) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	// A pending confirmation swallows the next key, whatever it is: the
	// prompt asked a yes/no question, so treating an unrelated keystroke as
	// anything but "no" would defeat the point of asking.
	if m.pending != nil {
		return m.resolveConfirm(msg)
	}
	switch msg.String() {
	case "q", keyInterrupt, keyEscape:
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
		return m.requestAction(actionStart)
	case "h":
		return m.requestAction(actionStop)
	case "S":
		return m.openSwitchModal()
	case "R":
		return m.requestAction(actionRestart)
	case "e":
		return m.requestAction(actionEnable)
	case "d":
		return m.requestAction(actionDisable)
	}
	return m, nil
}

// criticalDaemons carry this machine's DNS and outbound HTTP. Taking one
// down does not degrade the setup, it disconnects the machine - including
// the TUI's own ability to tell you what went wrong, and any remote session
// you might have used to put it back. They are therefore the rows where a
// destructive action asks first.
//
// Restart is not destructive in this sense: it ends with the daemon running.
// Only stop and disable leave it down, and disable additionally survives a
// reboot, which is how "the network broke and stayed broken" happens.
var criticalDaemons = map[string]bool{
	appAlpaca:  true,
	appAdGuard: true,
	appPrivoxy: true,
}

// pendingAction is an action held back until the operator confirms it.
type pendingAction struct {
	verb  string
	index int
}

// requestAction runs verb, or - when it would disconnect the machine - parks
// it behind a y/n prompt first.
//
// This exists because the whole keymap is single-key and unmodified: the
// cursor starts on row 0, which is alpaca, so one stray keystroke on a
// freshly opened TUI used to be enough to stop the proxy every outbound
// connection goes through, with no confirmation and no undo once the network
// it just removed was the one you needed.
func (m observeModel) requestAction(verb string) (tea.Model, tea.Cmd) {
	if !isDestructiveVerb(verb) || m.cursor >= len(m.rows) || !criticalDaemons[m.rows[m.cursor].name] {
		return m, m.actionCmd(verb)
	}
	row := m.rows[m.cursor]
	m.pending = &pendingAction{verb: verb, index: m.cursor}
	m.message = fmt.Sprintf(
		"%s %s? %s - press y to confirm, any other key to cancel",
		displayVerb(verb), row.name, criticalDaemonWarning(row.name),
	)
	m.messageIsErr = true
	return m, nil
}

func isDestructiveVerb(verb string) bool {
	return verb == actionStop || verb == actionDisable
}

// criticalDaemonWarning says what specifically breaks, rather than a generic
// "are you sure": the three daemons fail in visibly different ways, and
// knowing which one you are about to lose is the point of the prompt.
func criticalDaemonWarning(name string) string {
	switch name {
	case appAlpaca:
		return "every proxied outbound connection on this machine goes through it"
	case appAdGuard:
		return "it is this machine's resolver, so DNS stops"
	case appPrivoxy:
		return "direct contexts forward through it, so they lose outbound HTTP"
	}
	return "it is required for network connectivity"
}

// resolveConfirm consumes the answer to a pending confirmation. Only a
// literal "y" proceeds; the cursor is restored to the row that was asked
// about, so a confirmation cannot be applied to a different daemon than the
// one named in the prompt.
func (m observeModel) resolveConfirm(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	pending := *m.pending
	m.pending = nil
	if msg.String() != "y" {
		m.message = fmt.Sprintf("%s %s cancelled", displayVerb(pending.verb), m.rows[pending.index].name)
		m.messageIsErr = false
		return m, nil
	}
	m.cursor = pending.index
	m.message = ""
	m.messageIsErr = false
	return m, m.actionCmd(pending.verb)
}

// displayVerb is the wording the TUI uses for an action. The launchd verb
// stays "stop" everywhere it is a protocol value (config, systemDaemonActionCmd,
// launchdStop); "halt" is only what the operator reads, matching the h key.
func displayVerb(verb string) string {
	if verb == actionStop {
		return "halt"
	}
	return verb
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
// for a password) and suspends the TUI for the duration - unless the row
// configures an override command for verb (DaemonConfig.StartCommand etc.),
// in which case that runs instead; see systemDaemonActionCmd.
func (m observeModel) actionCmd(verb string) tea.Cmd {
	index := m.cursor
	row := m.rows[index]
	if strings.TrimSpace(row.label) == "" {
		err := fmt.Errorf("no launchd label configured for %s", row.name)
		return func() tea.Msg { return actionResultMsg{index: index, verb: verb, err: err} }
	}
	if normalizeDaemonScope(row.scope) == daemonScopeSystem {
		cmd, err := systemDaemonActionCmd(verb, row.label, row.cfgDaemon)
		if err != nil {
			return func() tea.Msg { return actionResultMsg{index: index, verb: verb, err: err} }
		}
		return tea.ExecProcess(cmd, func(err error) tea.Msg {
			return actionResultMsg{index: index, verb: verb, err: err}
		})
	}
	if override := daemonActionOverride(verb, row.cfgDaemon); strings.TrimSpace(override) != "" {
		return func() tea.Msg {
			return actionResultMsg{index: index, verb: verb, err: runDaemonOverrideCommand(override)}
		}
	}
	action, ok := launchdActions[verb]
	if !ok {
		err := fmt.Errorf("unknown action %q", verb)
		return func() tea.Msg { return actionResultMsg{index: index, verb: verb, err: err} }
	}
	return func() tea.Msg {
		return actionResultMsg{index: index, verb: verb, err: action(row.label, row.scope)}
	}
}

// launchdActions maps an observe verb to the launchctl operation behind it.
//
// It is a var, and the only path from actionCmd to launchd, so a test can
// replace it wholesale with stubs. That is not a convenience: the alpaca row
// takes its label from launchAgentService(), not from config, so it is
// always macswitcher's own real launch agent no matter what Config a test
// builds. A test that executed the tea.Cmd actionCmd returns therefore used
// to stop the operator's actual proxy - every `go test` run took the machine
// off the network. Tests must call stubLaunchdActions.
var launchdActions = map[string]func(label, scope string) error{
	actionStart:   launchdStart,
	actionStop:    launchdStop,
	actionRestart: launchdRestart,
	actionEnable:  launchdEnable,
	actionDisable: launchdDisable,
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
	case keyEscape, "q", "l", keyInterrupt:
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
