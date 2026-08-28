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
	Unbound         UnboundConfig                  `yaml:"unbound" mapstructure:"unbound"`
	AdGuard         AdGuardConfig                  `yaml:"adguard" mapstructure:"adguard"`
	Daemons         DaemonsConfig                  `yaml:"daemons" mapstructure:"daemons"`
	Applications    map[string]ApplicationCommands `yaml:"applications" mapstructure:"applications"`
	Contexts        map[string]SwitchContext       `yaml:"-" mapstructure:"-"`
}

type LocalProxyConfig struct {
	Host    string   `yaml:"host" mapstructure:"host"`
	Port    int      `yaml:"port" mapstructure:"port"`
	NoProxy []string `yaml:"no_proxy" mapstructure:"no_proxy"`
}

type SwitchContext struct {
	MacOSNetworkLocation string                `yaml:"mac_network_location" mapstructure:"mac_network_location"`
	DNS                  ContextDNSConfig      `yaml:"dns" mapstructure:"dns"`
	UnboundForwarders    []string              `yaml:"unbound_forwarders" mapstructure:"unbound_forwarders"`
	ProxyMode            string                `yaml:"proxy_mode" mapstructure:"proxy_mode"`
	ForwarderProxy       *ForwarderProxyConfig `yaml:"forwarder_proxy,omitempty" mapstructure:"forwarder_proxy"`
	Alpaca               *AlpacaConfig         `yaml:"alpaca,omitempty" mapstructure:"alpaca"`
	Apps                 LifecycleConfig       `yaml:"apps" mapstructure:"apps"`
}

type DNSConfig struct {
	LocalResolver string `yaml:"local_resolver" mapstructure:"local_resolver"`
}

type ContextDNSConfig struct {
	NetworkServices []string `yaml:"network_services" mapstructure:"network_services"`
	Resolvers       []string `yaml:"resolvers" mapstructure:"resolvers"`
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

type UnboundConfig struct {
	ForwardersFile string `yaml:"forwarders_file" mapstructure:"forwarders_file"`
}

// AdGuardConfig points at AdGuard Home's upstream_dns_file, the equivalent of
// unbound's forwarders.conf. Leave UpstreamsFile empty and macswitcher ignores
// AdGuard Home entirely, which is what every machine that has not opted into
// the trial wants.
//
// A context's UnboundForwarders drive both files: they are the same decision
// ("which upstream resolvers does this network want"), just written in two
// syntaxes.
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
	Unbound           DaemonConfig `yaml:"unbound" mapstructure:"unbound"`
	AdGuardHome       DaemonConfig `yaml:"adguardhome" mapstructure:"adguardhome"`
	KerberosKeepAlive DaemonConfig `yaml:"kerberos_keep_alive" mapstructure:"kerberos_keep_alive"`
	OMT               DaemonConfig `yaml:"omt" mapstructure:"omt"`
	VPN               DaemonConfig `yaml:"vpn" mapstructure:"vpn"`
	Tunneling         DaemonConfig `yaml:"tunneling" mapstructure:"tunneling"`
}

type ForwarderProxyConfig struct {
	ProxyServer             string   `yaml:"proxy_server" mapstructure:"proxy_server"`
	Port                    int      `yaml:"port" mapstructure:"port"`
	Username                string   `yaml:"username,omitempty" mapstructure:"username"`
	PasswordKeychainService string   `yaml:"password_keychain_service,omitempty" mapstructure:"password_keychain_service"`
	PasswordKeychainAccount string   `yaml:"password_keychain_account,omitempty" mapstructure:"password_keychain_account"`
	TicketFile              string   `yaml:"ticket_file,omitempty" mapstructure:"ticket_file"`
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
	appAlpaca  = "alpaca"
	appUnbound = "unbound"
	// appAdGuard is AdGuard Home, on trial as unbound's replacement. Both can
	// run at once (unbound on 127.0.0.2, AdGuard Home on 127.0.0.3), so a
	// context switch feeds whichever of them is configured.
	appAdGuard = "adguardhome"
)

// Loopback addresses. mDNSResponder owns 127.0.0.1:53, so unbound listens on
// 127.0.0.2 instead - see the unbound notes in MacbookSetup/Setup.md.
const (
	loopbackLocal    = "127.0.0.1"
	loopbackResolver = "127.0.0.2"
	// loopbackAdGuard is where AdGuard Home listens while it is being
	// evaluated next to unbound. Point dns.local_resolver here to try it.
	loopbackAdGuard = "127.0.0.3"
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
			NoProxy: []string{"localhost", loopbackLocal, "::1", "kubernetes"},
		},
		Alpaca: AlpacaConfig{
			Enabled: true,
			Command: []string{appAlpaca, "-l", placeholderLocalHost, "-p", placeholderLocalPort, "-C", placeholderPACFile},
		},
		Contexts: map[string]SwitchContext{
			contextHome: {
				MacOSNetworkLocation: currentNetworkLocation(),
				DNS:                  ContextDNSConfig{NetworkServices: currentNetworkServices(), Resolvers: []string{loopbackResolver}},
				ProxyMode:            "direct",
				Apps: LifecycleConfig{
					Restart: []string{"docker"},
					Reload:  []string{appUnbound},
				},
			},
			"work": {},
		},
		NetworkServices: nil,
		DNS: DNSConfig{
			LocalResolver: loopbackResolver,
		},
		Unbound: UnboundConfig{
			ForwardersFile: "/opt/homebrew/etc/unbound/conf.d/forwarders.conf",
		},
		// Empty on purpose: AdGuard Home is opt-in while it is on trial. Set
		// upstreams_file (and daemons.adguardhome.label) to bring it in.
		AdGuard: AdGuardConfig{},
		Applications: map[string]ApplicationCommands{
			appUnbound: {
				Reload:  []string{"unbound-control", actionReload},
				Restart: []string{"sudo", "launchctl", "kickstart", "-k", "system/net.unbound"},
			},
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
	homeContext.UnboundForwarders = currentUnboundForwarders(cfg.Unbound.ForwardersFile)
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
func currentAdGuardUpstreams(path string) (defaults, specific []string) {
	b, err := os.ReadFile(path) // #nosec G304 -- path is the configured AdGuard Home upstreams file, an operator-controlled setting
	if err != nil {
		return nil, nil
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
	return defaults, specific
}

func currentUnboundForwarders(path string) []string {
	b, err := os.ReadFile(path) // #nosec G304 -- path is the configured Unbound forwarders file, an operator-controlled setting
	if err != nil {
		return nil
	}
	var forwarders []string
	for line := range strings.SplitSeq(string(b), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "forward-addr:") {
			continue
		}
		forwarder := strings.TrimSpace(strings.TrimPrefix(line, "forward-addr:"))
		if forwarder != "" {
			forwarders = append(forwarders, forwarder)
		}
	}
	return forwarders
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
	if cfg.DNS.LocalResolver == "" {
		cfg.DNS.LocalResolver = loopbackResolver
	}
	if cfg.Unbound.ForwardersFile == "" {
		cfg.Unbound.ForwardersFile = "/opt/homebrew/etc/unbound/conf.d/forwarders.conf"
	}
	if cfg.Daemons.Unbound.Label == "" {
		cfg.Daemons.Unbound.Label = "homebrew.mxcl.unbound"
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
