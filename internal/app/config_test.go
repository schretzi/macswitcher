package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestInitConfigCreatesGlobalAndContextFiles(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	globalPath := filepath.Join(root, "config.yaml")
	if err := initConfig(globalPath); err != nil {
		t.Fatalf("initConfig() error = %v", err)
	}

	for _, name := range []string{"config.yaml", "state.json", "contexts/home.yaml", "contexts/work.yaml"} {
		if _, err := os.Stat(filepath.Join(root, name)); err != nil {
			t.Fatalf("expected %s: %v", name, err)
		}
	}

	cfg, err := loadConfig(globalPath)
	if err != nil {
		t.Fatalf("loadConfig() error = %v", err)
	}
	if got := cfg.DNS.LocalResolver; got != loopbackResolver {
		t.Fatalf("default resolver = %q, want %q", got, loopbackResolver)
	}
	if got := cfg.Contexts[contextHome].DNS.Resolvers; len(got) != 1 || got[0] != loopbackResolver {
		t.Fatalf("home resolvers = %#v, want the loopback resolver", got)
	}
	if len(cfg.Alpaca.Command) == 0 {
		t.Fatal("global Alpaca command is empty")
	}
	if got := cfg.AdGuard.UpstreamsFile; got != "/etc/adguardhome/upstreams.conf" {
		t.Fatalf("default adguard upstreams file = %q", got)
	}
	if got := strings.Join(cfg.LocalProxy.NoProxy, ","); !strings.Contains(got, "kubernetes") {
		t.Fatalf("default no_proxy = %q, want kubernetes", got)
	}
	if got := cfg.Applications["docker"].Restart; len(got) == 0 {
		t.Fatal("docker restart command is empty")
	}
	if got := cfg.Applications["streamdeck"].Start; strings.Join(got, " ") != "open -a Elgato Stream Deck" {
		t.Fatalf("Stream Deck start command = %#v", got)
	}
	if got := cfg.Applications["streamdeck"].Stop; len(got) == 0 {
		t.Fatal("Stream Deck stop command is empty")
	}
	if cfg.Contexts["work"].MacOSNetworkLocation != "" || cfg.Contexts["work"].ProxyMode != "" {
		t.Fatalf("work context is not empty: %#v", cfg.Contexts["work"])
	}
}

func TestBuildProxyCommandWithoutForwarderUsesDirectCommand(t *testing.T) {
	t.Parallel()

	cfg := Config{
		CurrentContext: contextHome,
		LocalProxy:     LocalProxyConfig{Host: loopbackLocal, Port: 3128},
		Contexts: map[string]SwitchContext{
			contextHome: {ProxyMode: "direct"},
		},
	}

	args, err := buildProxyCommand(cfg, AlpacaConfig{
		Enabled: true,
		Command: []string{appAlpaca, "-l", placeholderLocalHost, "-p", placeholderLocalPort, "-C", placeholderPACFile},
	})
	if err != nil {
		t.Fatalf("buildProxyCommand() error = %v", err)
	}
	want := []string{appAlpaca, "-l", loopbackLocal, "-p", "3128"}
	if strings.Join(args, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("buildProxyCommand() = %#v, want %#v", args, want)
	}
}

func TestBuildProxyCommandUsesInstalledAlpacaPath(t *testing.T) {
	t.Setenv("MACSWITCHER_ALPACA_BINARY", "/opt/homebrew/bin/alpaca")

	cfg := Config{
		CurrentContext: contextHome,
		LocalProxy:     LocalProxyConfig{Host: loopbackLocal, Port: 3128},
		Contexts: map[string]SwitchContext{
			contextHome: {ProxyMode: "direct"},
		},
	}
	args, err := buildProxyCommand(cfg, AlpacaConfig{
		Enabled: true,
		Command: []string{appAlpaca, "-p", placeholderLocalPort},
	})
	if err != nil {
		t.Fatalf("buildProxyCommand() error = %v", err)
	}
	if args[0] != "/opt/homebrew/bin/alpaca" {
		t.Fatalf("alpaca executable = %q, want installed path", args[0])
	}
}

func TestRunApplicationActionFallsBackToStopAndStart(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		action string
	}{
		{name: actionRestart, action: actionRestart},
		{name: actionReload, action: actionReload},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			outputPath := filepath.Join(t.TempDir(), "actions")
			commands := ApplicationCommands{
				Start: []string{"sh", "-c", "printf start >> \"$1\"", "sh", outputPath},
				Stop:  []string{"sh", "-c", "printf stop >> \"$1\"", "sh", outputPath},
			}
			if err := runApplicationAction("test", tt.action, commands); err != nil {
				t.Fatalf("runApplicationAction() error = %v", err)
			}
			got, err := os.ReadFile(outputPath)
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != "stopstart" {
				t.Fatalf("action order = %q, want %q", got, "stopstart")
			}
		})
	}
}

// The rename from unbound_forwarders to upstreams has to fail loudly: a
// context left on the old key would switch networks and change no upstream at
// all, which looks exactly like a working switch.
func TestLoadConfigRejectsRenamedUnboundForwardersKey(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "contexts"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte("current_context: home\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	body := "unbound_forwarders:\n  - 192.0.2.1\n"
	if err := os.WriteFile(filepath.Join(dir, "contexts", "home.yaml"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	_, err := loadConfig(filepath.Join(dir, "config.yaml"))
	if err == nil {
		t.Fatal("loadConfig() accepted the removed unbound_forwarders key")
	}
	if !strings.Contains(err.Error(), "upstreams") {
		t.Fatalf("error does not name the new key: %v", err)
	}
}

func writeMinimalConfig(t *testing.T, dnsBody string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "contexts"), 0o755); err != nil {
		t.Fatal(err)
	}
	body := "current_context: home\nlocal_proxy:\n  host: 127.0.0.1\n  port: 3128\n"
	if dnsBody != "" {
		body += dnsBody
	}
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "contexts", "home.yaml"), []byte("proxy_mode: direct\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return filepath.Join(dir, "config.yaml")
}

// An empty (or absent) dns.backend must normalize to AdGuard Home, so every
// config written before dns.backend existed keeps behaving exactly as it did.
func TestLoadConfigDefaultsDNSBackendToAdGuard(t *testing.T) {
	t.Parallel()

	cfg, err := loadConfig(writeMinimalConfig(t, ""))
	if err != nil {
		t.Fatalf("loadConfig() error = %v", err)
	}
	if got := dnsBackend(cfg); got != dnsBackendAdGuard {
		t.Fatalf("dnsBackend() = %q, want %q", got, dnsBackendAdGuard)
	}
}

func TestLoadConfigAcceptsUnboundBackend(t *testing.T) {
	t.Parallel()

	cfg, err := loadConfig(writeMinimalConfig(t, "dns:\n  backend: unbound\n"))
	if err != nil {
		t.Fatalf("loadConfig() error = %v", err)
	}
	if got := dnsBackend(cfg); got != dnsBackendUnbound {
		t.Fatalf("dnsBackend() = %q, want %q", got, dnsBackendUnbound)
	}
}

// A typo in dns.backend must fail loudly rather than silently fall back to
// AdGuard Home - the same reasoning as rejecting unbound_forwarders: a
// context switch that quietly targets the wrong (or no) resolver looks like
// it worked.
func TestLoadConfigRejectsInvalidDNSBackend(t *testing.T) {
	t.Parallel()

	_, err := loadConfig(writeMinimalConfig(t, "dns:\n  backend: bind9\n"))
	if err == nil {
		t.Fatal("loadConfig() accepted an invalid dns.backend value")
	}
	if !strings.Contains(err.Error(), "dns.backend") {
		t.Fatalf("error does not name dns.backend: %v", err)
	}
}
