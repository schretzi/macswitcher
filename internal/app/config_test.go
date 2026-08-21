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
	if got := cfg.DNS.LocalResolver; got != "127.0.0.2" {
		t.Fatalf("default resolver = %q, want %q", got, "127.0.0.2")
	}
	if got := cfg.Contexts["home"].DNS.Resolvers; len(got) != 1 || got[0] != "127.0.0.2" {
		t.Fatalf("home resolvers = %#v, want [127.0.0.2]", got)
	}
	if len(cfg.Alpaca.Command) == 0 {
		t.Fatal("global Alpaca command is empty")
	}
	if got := cfg.Applications["unbound"].Reload; len(got) != 2 || got[0] != "unbound-control" || got[1] != "reload" {
		t.Fatalf("unbound reload command = %#v, want unbound-control reload", got)
	}
	if got := cfg.Applications["unbound"].Restart; strings.Join(got, " ") != "sudo launchctl kickstart -k system/net.unbound" {
		t.Fatalf("unbound restart command = %#v, want LaunchDaemon kickstart", got)
	}
	if got := cfg.Contexts["home"].Apps.Reload; len(got) != 1 || got[0] != "unbound" {
		t.Fatalf("home reload applications = %#v, want [unbound]", got)
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
		CurrentContext: "home",
		LocalProxy:     LocalProxyConfig{Host: "127.0.0.1", Port: 3128},
		Contexts: map[string]SwitchContext{
			"home": {ProxyMode: "direct"},
		},
	}

	args, err := buildProxyCommand(cfg, AlpacaConfig{
		Enabled: true,
		Command: []string{"alpaca", "-l", "{{local_host}}", "-p", "{{local_port}}", "-C", "{{pac_file}}"},
	})
	if err != nil {
		t.Fatalf("buildProxyCommand() error = %v", err)
	}
	want := []string{"alpaca", "-l", "127.0.0.1", "-p", "3128"}
	if strings.Join(args, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("buildProxyCommand() = %#v, want %#v", args, want)
	}
}

func TestBuildProxyCommandUsesInstalledAlpacaPath(t *testing.T) {
	t.Setenv("MACSWITCHER_ALPACA_BINARY", "/opt/homebrew/bin/alpaca")

	cfg := Config{
		CurrentContext: "home",
		LocalProxy:     LocalProxyConfig{Host: "127.0.0.1", Port: 3128},
		Contexts: map[string]SwitchContext{
			"home": {ProxyMode: "direct"},
		},
	}
	args, err := buildProxyCommand(cfg, AlpacaConfig{
		Enabled: true,
		Command: []string{"alpaca", "-p", "{{local_port}}"},
	})
	if err != nil {
		t.Fatalf("buildProxyCommand() error = %v", err)
	}
	if args[0] != "/opt/homebrew/bin/alpaca" {
		t.Fatalf("alpaca executable = %q, want installed path", args[0])
	}
}

func TestWriteUnboundForwardersReplacesSymlink(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	target := filepath.Join(root, "repository-forwarders.conf")
	forwardersFile := filepath.Join(root, "conf.d", "forwarders.conf")
	targetContent := "forward-zone:\n  name: \".\"\n  forward-addr: 192.0.2.1\n"
	if err := os.WriteFile(target, []byte(targetContent), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := os.MkdirAll(filepath.Dir(forwardersFile), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, forwardersFile); err != nil {
		t.Fatal(err)
	}

	cfg := Config{Unbound: UnboundConfig{ForwardersFile: forwardersFile}}
	if err := writeUnboundForwarders(cfg, []string{"198.51.100.53"}); err != nil {
		t.Fatalf("writeUnboundForwarders() error = %v", err)
	}

	gotTarget, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(gotTarget) != targetContent {
		t.Fatalf("symlink target changed: got %q, want %q", gotTarget, targetContent)
	}
	info, err := os.Lstat(forwardersFile)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		t.Fatal("forwarders file is still a symlink")
	}
	got, err := os.ReadFile(forwardersFile)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), "forward-addr: 198.51.100.53") {
		t.Fatalf("forwarders file does not contain selected resolver: %q", got)
	}
}

func TestRunApplicationActionFallsBackToStopAndStart(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		action string
	}{
		{name: "restart", action: "restart"},
		{name: "reload", action: "reload"},
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
