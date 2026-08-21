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
	Kerberos             KerberosConfig        `yaml:"kerberos" mapstructure:"kerberos"`
	Apps                 AppLifecycleConfig    `yaml:"apps" mapstructure:"apps"`
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

type KerberosConfig struct {
	TicketFile    string `yaml:"ticket_file" mapstructure:"ticket_file"`
	UpstreamProxy string `yaml:"upstream_proxy" mapstructure:"upstream_proxy"`
}

type AppLifecycleConfig struct {
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

type ForwarderProxyConfig struct {
	ProxyServer             string   `yaml:"proxy_server" mapstructure:"proxy_server"`
	Port                    int      `yaml:"port" mapstructure:"port"`
	Username                string   `yaml:"username" mapstructure:"username"`
	PasswordKeychainService string   `yaml:"password_keychain_service" mapstructure:"password_keychain_service"`
	PasswordKeychainAccount string   `yaml:"password_keychain_account" mapstructure:"password_keychain_account"`
	PacFile                 string   `yaml:"pac_file" mapstructure:"pac_file"`
	AuthAllowlist           []string `yaml:"auth_allowlist" mapstructure:"auth_allowlist"`
}

const (
	defaultConfigRelPath = ".config/macswitcher/config.yaml"
	runtimeStateFile     = "state.json"
	launchAgentLabel     = "com.macswitcher.proxy"
)

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
		CurrentContext: "home",
		LocalProxy: LocalProxyConfig{
			Host:    "127.0.0.1",
			Port:    3128,
			NoProxy: []string{"localhost", "127.0.0.1", "::1", "kubernetes"},
		},
		Alpaca: AlpacaConfig{
			Enabled: true,
			Command: []string{"alpaca", "-l", "{{local_host}}", "-p", "{{local_port}}", "-C", "{{pac_file}}"},
		},
		Contexts: map[string]SwitchContext{
			"home": {
				MacOSNetworkLocation: currentNetworkLocation(),
				DNS:                  ContextDNSConfig{NetworkServices: currentNetworkServices(), Resolvers: []string{"127.0.0.2"}},
				ProxyMode:            "direct",
				Apps: AppLifecycleConfig{
					Restart: []string{"docker"},
					Reload:  []string{"unbound"},
				},
			},
			"work": {},
		},
		NetworkServices: nil,
		DNS: DNSConfig{
			LocalResolver: "127.0.0.2",
		},
		Unbound: UnboundConfig{
			ForwardersFile: "/opt/homebrew/etc/unbound/conf.d/forwarders.conf",
		},
		Applications: map[string]ApplicationCommands{
			"unbound": {
				Reload:  []string{"unbound-control", "reload"},
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
	homeContext := cfg.Contexts["home"]
	homeContext.UnboundForwarders = currentUnboundForwarders(cfg.Unbound.ForwardersFile)
	cfg.Contexts["home"] = homeContext
	if err := saveConfig(path, cfg); err != nil {
		return err
	}
	return saveRuntimeState(path, ConfigState{CurrentContext: "home"})
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
	for _, line := range strings.Split(out, "\n") {
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

func currentUnboundForwarders(path string) []string {
	b, err := os.ReadFile(path) // #nosec G304 -- path is the configured Unbound forwarders file, an operator-controlled setting
	if err != nil {
		return nil
	}
	var forwarders []string
	for _, line := range strings.Split(string(b), "\n") {
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

func loadConfig(path string) (Config, error) {
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
		cfg.DNS.LocalResolver = "127.0.0.2"
	}
	if cfg.Unbound.ForwardersFile == "" {
		cfg.Unbound.ForwardersFile = "/opt/homebrew/etc/unbound/conf.d/forwarders.conf"
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
