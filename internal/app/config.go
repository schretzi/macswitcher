package app

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/viper"
	"gopkg.in/yaml.v3"
)

type Config struct {
	CurrentContext  string                         `yaml:"-" mapstructure:"-"`
	LocalProxy      LocalProxyConfig               `yaml:"local_proxy" mapstructure:"local_proxy"`
	Alpaca          AlpacaConfig                   `yaml:"alpaca" mapstructure:"alpaca"`
	NetworkServices []string                       `yaml:"network_services" mapstructure:"network_services"`
	DNS             DNSConfig                      `yaml:"dns" mapstructure:"dns"`
	AdGuard         AdGuardConfig                  `yaml:"adguard" mapstructure:"adguard"`
	FilterProxy     FilterProxyConfig              `yaml:"filter_proxy" mapstructure:"filter_proxy"`
	Daemons         DaemonsConfig                  `yaml:"daemons" mapstructure:"daemons"`
	Applications    map[string]ApplicationCommands `yaml:"applications" mapstructure:"applications"`
	Contexts        map[string]SwitchContext       `yaml:"-" mapstructure:"-"`
}

type LocalProxyConfig struct {
	Host    string   `yaml:"host" mapstructure:"host"`
	Port    int      `yaml:"port" mapstructure:"port"`
	NoProxy []string `yaml:"no_proxy" mapstructure:"no_proxy"`
	// RestartAgents names entries in `applications` that read their proxy
	// from the launchd environment and therefore cannot notice a change on
	// their own. They are restarted after the proxy is applied, which is the
	// only point at which the new environment exists.
	//
	// This is deliberately not a per-context apps.restart list. Those run
	// early in a switch - before DNS and long before the proxy - so an agent
	// restarted there would inherit the environment of the context being left.
	// It also does not vary by context: an agent that consumes the proxy needs
	// the restart in every context, including the one that turns the proxy off.
	//
	// Restarting is required rather than merely convenient: Go caches the
	// environment on its first http.ProxyFromEnvironment call, so even a
	// running process that could see the new value would not use it.
	RestartAgents []string `yaml:"restart_agents,omitempty" mapstructure:"restart_agents"`
}

// FilterProxyConfig is the local URL-filtering proxy alpaca forwards to in
// direct contexts - privoxy on this machine. See internal/app/filterproxy.go
// for why enabling it means generating a PAC file.
type FilterProxyConfig struct {
	Enabled bool   `yaml:"enabled" mapstructure:"enabled"`
	Host    string `yaml:"host" mapstructure:"host"`
	Port    int    `yaml:"port" mapstructure:"port"`
	// FailOpen adds a DIRECT fallback to the generated PAC, so a filter that
	// is not listening costs filtering rather than connectivity. The honest
	// trade either way: fail-open browses unfiltered without saying so (which
	// is what `macswitcher status` is for), fail-closed takes the network
	// down with the filter.
	FailOpen bool `yaml:"fail_open" mapstructure:"fail_open"`
	// Direct names hosts and domains that bypass the filter, on top of
	// local_proxy.no_proxy. A leading dot matches subdomains.
	Direct []string `yaml:"direct,omitempty" mapstructure:"direct"`
	// PACFile overrides where the generated PAC is written. Empty means
	// ~/.local/state/macswitcher/filter.pac - it is generated state, not
	// something to edit.
	PACFile string `yaml:"pac_file,omitempty" mapstructure:"pac_file"`
}

type SwitchContext struct {
	MacOSNetworkLocation string           `yaml:"mac_network_location" mapstructure:"mac_network_location"`
	DNS                  ContextDNSConfig `yaml:"dns" mapstructure:"dns"`
	Upstreams            []string         `yaml:"upstreams" mapstructure:"upstreams"`
	// Rejected on load rather than ignored. This was unbound_forwarders, and
	// the same list now drives AdGuard Home's upstream_dns_file - a context
	// still carrying the old key would switch networks without changing a
	// single upstream, and nothing would say so.
	UnboundForwardersRemoved []string              `yaml:"unbound_forwarders,omitempty" mapstructure:"unbound_forwarders"`
	ProxyMode                string                `yaml:"proxy_mode" mapstructure:"proxy_mode"`
	ForwarderProxy           *ForwarderProxyConfig `yaml:"forwarder_proxy,omitempty" mapstructure:"forwarder_proxy"`
	Alpaca                   *AlpacaConfig         `yaml:"alpaca,omitempty" mapstructure:"alpaca"`
	Apps                     LifecycleConfig       `yaml:"apps" mapstructure:"apps"`
}

type DNSConfig struct {
	LocalResolver string `yaml:"local_resolver" mapstructure:"local_resolver"`
}

type ContextDNSConfig struct {
	NetworkServices []string `yaml:"network_services" mapstructure:"network_services"`
	Resolvers       []string `yaml:"resolvers" mapstructure:"resolvers"`
	// CheckHost overrides the name a switch resolves to decide whether DNS
	// works. The defaults - google.com, or the forward proxy's own hostname -
	// both assume the network resolves public names, and a corporate network
	// reached over a VPN does not have to: its resolvers may serve the
	// intranet only, which makes google.com a test of the wrong thing and
	// fails a switch that in fact worked. Point this at an intranet name that
	// is always resolvable on the network the context describes.
	CheckHost string `yaml:"check_host,omitempty" mapstructure:"check_host"`
	// SearchDomains is the DNS search list to apply, completing single-label
	// names. Needed when a corporate PAC nominates its proxy by short name
	// ("PROXY proxy:8080"), which nothing else can resolve. Left empty the
	// search list is cleared, so a suffix set for one context cannot leak
	// into the next.
	SearchDomains []string `yaml:"search_domains,omitempty" mapstructure:"search_domains"`
}

type AlpacaConfig struct {
	Enabled bool     `yaml:"enabled" mapstructure:"enabled"`
	Command []string `yaml:"command" mapstructure:"command"`
}

type LifecycleConfig struct {
	Restart []string `yaml:"restart" mapstructure:"restart"`
	Stop    []string `yaml:"stop" mapstructure:"stop"`
	Start   []string `yaml:"start" mapstructure:"start"`
	Reload  []string `yaml:"reload" mapstructure:"reload"`
}

type ApplicationCommands struct {
	Start   []string `yaml:"start,omitempty" mapstructure:"start"`
	Stop    []string `yaml:"stop,omitempty" mapstructure:"stop"`
	Restart []string `yaml:"restart,omitempty" mapstructure:"restart"`
	Reload  []string `yaml:"reload,omitempty" mapstructure:"reload"`
}

// AdGuardConfig points at AdGuard Home's upstream_dns_file, which a context's
// Upstreams are written into on every switch. Leave UpstreamsFile empty and
// macswitcher does not touch it.
//
// The per-domain "[/zone/]address" lines in that file belong to whoever put
// them there and are carried over untouched; only the default upstreams are
// macswitcher's.
type AdGuardConfig struct {
	UpstreamsFile string `yaml:"upstreams_file" mapstructure:"upstreams_file"`
}

// DaemonConfig identifies a launchd agent that `macswitcher observe` can show
// and control alongside macswitcher's own Alpaca agent. It is not installed
// by macswitcher itself: its plist is expected to already exist at the
// standard per-scope path (e.g. installed by Homebrew or by an external
// Ansible role). Leave Label empty to hide it from `observe` as "not
// configured".
type DaemonConfig struct {
	Label string `yaml:"label,omitempty" mapstructure:"label"`
	// Scope is "user" (default): a per-user LaunchAgent loaded in the gui/<uid>
	// domain from ~/Library/LaunchAgents/<label>.plist. Set to "system" for a
	// LaunchDaemon loaded in the system domain from
	// /Library/LaunchDaemons/<label>.plist; start/stop/enable/disable on a
	// system-scoped daemon run under sudo with an interactive password prompt.
	Scope string `yaml:"scope,omitempty" mapstructure:"scope"`
	// Interface is optional and only used by the vpn row: a network
	// interface name (e.g. "utun99") that observe checks for a live inet
	// address, since a running VPN supervisor process doesn't guarantee the
	// tunnel itself is actually up.
	Interface string `yaml:"interface,omitempty" mapstructure:"interface"`
}

// DaemonsConfig lists the external daemons `macswitcher observe` can show,
// beyond macswitcher's own Alpaca launch agent (always shown).
type DaemonsConfig struct {
	AdGuardHome       DaemonConfig `yaml:"adguardhome" mapstructure:"adguardhome"`
	Container         DaemonConfig `yaml:"container" mapstructure:"container"`
	KerberosKeepAlive DaemonConfig `yaml:"kerberos_keep_alive" mapstructure:"kerberos_keep_alive"`
	OMT               DaemonConfig `yaml:"omt" mapstructure:"omt"`
	VPN               DaemonConfig `yaml:"vpn" mapstructure:"vpn"`
	Tunneling         DaemonConfig `yaml:"tunneling" mapstructure:"tunneling"`
	Privoxy           DaemonConfig `yaml:"privoxy" mapstructure:"privoxy"`
	Lsrules           DaemonConfig `yaml:"lsrules" mapstructure:"lsrules"`
}

type ForwarderProxyConfig struct {
	ProxyServer             string   `yaml:"proxy_server" mapstructure:"proxy_server"`
	Port                    int      `yaml:"port" mapstructure:"port"`
	Username                string   `yaml:"username,omitempty" mapstructure:"username"`
	PasswordKeychainService string   `yaml:"password_keychain_service,omitempty" mapstructure:"password_keychain_service"`
	PasswordKeychainAccount string   `yaml:"password_keychain_account,omitempty" mapstructure:"password_keychain_account"`
	PacFile                 string   `yaml:"pac_file" mapstructure:"pac_file"`
	AuthAllowlist           []string `yaml:"auth_allowlist" mapstructure:"auth_allowlist"`
}

// Action names, as they appear in a context's `apps:` lists and in the
// `applications:` command map.
const (
	actionStart   = "start"
	actionStop    = "stop"
	actionRestart = "restart"
	actionReload  = "reload"
)

// Names of the managed applications macswitcher knows about by name, as used
// as keys in the `applications:` map.
const (
	appAlpaca = "alpaca"
	// appAdGuard is AdGuard Home, the resolver this machine runs. A context
	// switch rewrites its default upstreams and restarts it.
	appAdGuard = "adguardhome"
	// appContainer is Apple's container runtime, which kiac builds its cluster
	// node VMs on. Watched rather than driven: macswitcher never starts or
	// stops it, but a context switch is a common moment for it to be down.
	appContainer = "container"
	// appPrivoxy is the filtering proxy alpaca forwards to in direct
	// contexts. Like appContainer it is watched, not driven - but unlike it,
	// its absence is invisible without help: the generated PAC fails open, so
	// a filter that is down looks exactly like normal browsing.
	appPrivoxy = "privoxy"
	// appVPN is the employer VPN agent. Watched only - macswitcher never
	// dials it, but a context switch is a common moment for it to matter.
	appVPN = "vpn"
	// appLsrules serves the Little Snitch rule groups over HTTPS. Watched,
	// not driven - but its failure mode is quiet: Little Snitch keeps the
	// rules it already has, so a subscription that stopped refreshing looks
	// exactly like one that is working.
	appLsrules = "lsrules"
)

// defaultFilterProxyPort is privoxy's own default listen port.
const defaultFilterProxyPort = 8118

// Loopback addresses. mDNSResponder owns 127.0.0.1:53, so the local resolver
// listens on another loopback alias - AdGuard Home uses 127.0.0.3 for both DNS
// and its web UI. The aliases are created at boot by com.schretzi.localhost-alias
// (MacbookSetup's localhost_alias role); see MacbookSetup/Setup.md -> DNS.
//
// This is only the default written into a fresh config. A machine pointing
// somewhere else sets dns.local_resolver.
const (
	loopbackLocal    = "127.0.0.1"
	loopbackResolver = "127.0.0.3"
	// loopbackIPv6 appears in every no_proxy list and in the generated PAC's
	// direct rules.
	loopbackIPv6 = "::1"
	// hostKubernetes is the /etc/hosts alias for the cluster API server, in
	// the default no_proxy list because it resolves only on this machine.
	hostKubernetes = "kubernetes"
	// PROXY_STATE values in ~/.zsh/rcs/proxy. "off" is spelled by
	// ProxyModeOff, which happens to be the same word.
	proxyStateOn       = "on"
	proxyStateFiltered = "filtered"
)

// contextHome is the context `config init` seeds and falls back to.
const contextHome = "home"

// Placeholders substituted into a context's alpaca command line.
const (
	placeholderLocalHost = "{{local_host}}"
	placeholderLocalPort = "{{local_port}}"
	placeholderPACFile   = "{{pac_file}}"
)

const (
	defaultConfigRelPath = ".config/macswitcher/config.yaml"
	runtimeStateFile     = "state.json"

	// ProxyModeOff removes all proxy configuration (shell, system settings, Docker, ...).
	ProxyModeOff = "off"
	// ProxyModeDirect points system settings at the local proxy, which then reaches the
	// internet directly without an upstream forwarder.
	ProxyModeDirect = "direct"
	// ProxyModeForward points system settings at the local proxy, which then forwards
	// requests to the upstream proxy defined in forwarder_proxy.
	ProxyModeForward = "forward"
)

// validProxyModes lists the only accepted values for a context's proxy_mode.
var validProxyModes = []string{ProxyModeOff, ProxyModeDirect, ProxyModeForward}

// isValidProxyMode reports whether mode (case-insensitive) is one of off, direct, or forward.
func isValidProxyMode(mode string) bool {
	for _, valid := range validProxyModes {
		if strings.EqualFold(mode, valid) {
			return true
		}
	}
	return false
}

// isForwardProxyMode reports whether mode (case-insensitive) is "forward".
func isForwardProxyMode(mode string) bool {
	return strings.EqualFold(mode, ProxyModeForward)
}

func configPath() (string, error) {
	if custom := strings.TrimSpace(os.Getenv("MACSWITCHER_CONFIG")); custom != "" {
		return custom, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	newPath := filepath.Join(home, defaultConfigRelPath)
	return newPath, nil
}

func contextsPath(globalPath string) string {
	return filepath.Join(filepath.Dir(globalPath), "contexts")
}

func statePath(globalPath string) string {
	return filepath.Join(filepath.Dir(globalPath), runtimeStateFile)
}

func initConfig(path string) error {
	if _, err := os.Stat(path); err == nil {
		return fmt.Errorf("config already exists at %s", path)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return err
	}
	cfg := Config{
		CurrentContext: contextHome,
		LocalProxy: LocalProxyConfig{
			Host:    loopbackLocal,
			Port:    3128,
			NoProxy: []string{"localhost", loopbackLocal, loopbackIPv6, hostKubernetes},
		},
		Alpaca: AlpacaConfig{
			Enabled: true,
			Command: []string{appAlpaca, "-l", placeholderLocalHost, "-p", placeholderLocalPort, "-C", placeholderPACFile},
		},
		// Off by default: it needs a filtering proxy actually listening on
		// the port, and a machine without one would otherwise generate a PAC
		// pointing at nothing.
		FilterProxy: FilterProxyConfig{
			Enabled:  false,
			Host:     loopbackLocal,
			Port:     defaultFilterProxyPort,
			FailOpen: true,
		},
		Contexts: map[string]SwitchContext{
			contextHome: {
				MacOSNetworkLocation: currentNetworkLocation(),
				DNS:                  ContextDNSConfig{NetworkServices: currentNetworkServices(), Resolvers: []string{loopbackResolver}},
				ProxyMode:            "direct",
				Apps: LifecycleConfig{
					Restart: []string{"docker"},
				},
			},
			"work": {},
		},
		NetworkServices: nil,
		DNS: DNSConfig{
			LocalResolver: loopbackResolver,
		},
		AdGuard: AdGuardConfig{
			UpstreamsFile: "/etc/adguardhome/upstreams.conf",
		},
		Applications: map[string]ApplicationCommands{
			"docker": {
				Start:   []string{"open", "-a", "Docker"},
				Stop:    []string{"osascript", "-e", `tell application "Docker" to quit`},
				Restart: []string{"sh", "-c", `osascript -e 'tell application "Docker" to quit' && open -a Docker`},
			},
			"streamdeck": {
				Start: []string{"open", "-a", "Elgato Stream Deck"},
				Stop:  []string{"osascript", "-e", `tell application "Elgato Stream Deck" to quit`},
			},
		},
	}
	homeContext := cfg.Contexts[contextHome]
	homeContext.Upstreams, _, _ = currentAdGuardUpstreams(cfg.AdGuard.UpstreamsFile)
	cfg.Contexts[contextHome] = homeContext
	if err := saveConfig(path, cfg); err != nil {
		return err
	}
	return saveRuntimeState(path, ConfigState{CurrentContext: contextHome})
}

type ConfigState struct {
	CurrentContext string `json:"current_context"`
}

func loadRuntimeState(path string) (ConfigState, error) {
	var state ConfigState
	b, err := os.ReadFile(statePath(path))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return state, nil
		}
		return state, err
	}
	if err := json.Unmarshal(b, &state); err != nil {
		return state, fmt.Errorf("decode runtime state: %w", err)
	}
	return state, nil
}

func saveRuntimeState(path string, state ConfigState) error {
	b, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(statePath(path), append(b, '\n'), 0o600)
}

func currentNetworkLocation() string {
	out, err := runCommandOutput("scselect")
	if err != nil {
		return "Automatic"
	}
	for line := range strings.SplitSeq(out, "\n") {
		if strings.Contains(line, "*") {
			return strings.Trim(strings.TrimSpace(strings.TrimPrefix(line, "*")), "\"")
		}
	}
	return "Automatic"
}

func currentNetworkServices() []string {
	services, err := listNetworkServices()
	if err != nil {
		return nil
	}

	return services
}

// currentAdGuardUpstreams reads back AdGuard Home's upstream_dns_file, split
// into the plain default upstreams and the domain-specific "[/zone/]addr"
// entries. AdGuard Home treats a line as a comment only when it starts with
// '#', so that is the only comment form to skip.
func currentAdGuardUpstreams(path string) (defaults, specific []string, err error) {
	b, err := os.ReadFile(path) // #nosec G304 -- path is the configured AdGuard Home upstreams file, an operator-controlled setting
	if err != nil {
		// Distinguished from "the file is empty" on purpose: AdGuard Home
		// forces its own work directory to 0700, so a file placed there is
		// unreadable to this process and would otherwise be reported as
		// "no upstreams" for ever - a wrong answer that looks like a real one.
		return nil, nil, err
	}
	for line := range strings.SplitSeq(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "[") {
			specific = append(specific, line)
			continue
		}
		defaults = append(defaults, line)
	}
	return defaults, specific, nil
}

func loadConfig(path string) (Config, error) { //nolint:gocyclo // TODO: split this up. Left as-is for now because it drives live network/VPN/proxy switching and a refactor needs its own test pass.
	cfg, err := readConfigFile(path)
	if err != nil {
		return cfg, err
	}
	state, err := loadRuntimeState(path)
	if err != nil {
		return cfg, err
	}
	if state.CurrentContext != "" {
		cfg.CurrentContext = state.CurrentContext
	}
	cfg.Contexts = make(map[string]SwitchContext)
	entries, err := os.ReadDir(contextsPath(path))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return cfg, fmt.Errorf("read contexts: %w", err)
	}
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".yaml" {
			continue
		}
		contextName := strings.TrimSuffix(entry.Name(), ".yaml")
		context, err := readContextFile(filepath.Join(contextsPath(path), entry.Name()))
		if err != nil {
			return cfg, fmt.Errorf("decode context %q: %w", contextName, err)
		}
		cfg.Contexts[contextName] = context
	}
	if len(cfg.Contexts) == 0 {
		return cfg, errors.New("no context files found")
	}
	// Loudly, not silently. unbound is gone and the same list drives AdGuard
	// Home's upstream_dns_file now, so a context left on the old key would
	// switch networks without changing a single upstream - working, quiet, and
	// forwarding to the previous network's resolvers.
	for name, context := range cfg.Contexts {
		if len(context.UnboundForwardersRemoved) > 0 {
			return cfg, fmt.Errorf(
				"contexts.%s still uses unbound_forwarders; rename the key to upstreams "+
					"(unbound has been replaced by AdGuard Home, and the same list is now "+
					"written to its upstream_dns_file)", name,
			)
		}
	}
	if cfg.DNS.LocalResolver == "" {
		cfg.DNS.LocalResolver = loopbackResolver
	}
	if cfg.LocalProxy.Host == "" || cfg.LocalProxy.Port <= 0 {
		return cfg, errors.New("invalid local_proxy values")
	}
	if len(cfg.Contexts) == 0 {
		return cfg, errors.New("contexts cannot be empty")
	}
	if cfg.CurrentContext == "" {
		for k := range cfg.Contexts {
			cfg.CurrentContext = k
			break
		}
	}
	if _, ok := cfg.Contexts[cfg.CurrentContext]; !ok {
		return cfg, fmt.Errorf("current_context %q does not exist in contexts", cfg.CurrentContext)
	}
	return cfg, nil
}

func readConfigFile(path string) (Config, error) {
	var cfg Config
	settings := viper.New()
	settings.SetConfigFile(path)
	settings.SetEnvPrefix("MACSWITCHER")
	settings.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))
	settings.AutomaticEnv()
	if err := settings.ReadInConfig(); err != nil {
		return cfg, err
	}
	if err := settings.Unmarshal(&cfg); err != nil {
		return cfg, fmt.Errorf("decode config: %w", err)
	}
	return cfg, nil
}

func readContextFile(path string) (SwitchContext, error) {
	var context SwitchContext
	settings := viper.New()
	settings.SetConfigFile(path)
	if err := settings.ReadInConfig(); err != nil {
		return context, err
	}
	if err := settings.Unmarshal(&context); err != nil {
		return context, err
	}
	return context, nil
}

func saveConfig(path string, cfg Config) error {
	b, err := yaml.Marshal(cfg)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return err
	}
	if err := os.WriteFile(path, append(b, '\n'), 0o600); err != nil {
		return err
	}
	if err := os.MkdirAll(contextsPath(path), 0o750); err != nil {
		return err
	}
	for name, context := range cfg.Contexts {
		contextBytes, err := yaml.Marshal(context)
		if err != nil {
			return fmt.Errorf("encode context %q: %w", name, err)
		}
		contextPath := filepath.Join(contextsPath(path), name+".yaml")
		if err := os.WriteFile(contextPath, append(contextBytes, '\n'), 0o600); err != nil {
			return fmt.Errorf("write context %q: %w", name, err)
		}
	}
	fmt.Printf("saved config: %s\n", path)
	return nil
}
