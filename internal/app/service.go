package app

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

func serviceInstall() error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	if isGoRunExecutable(exe) {
		stablePath := filepath.Join(home, ".local", "bin", "macswitcher")
		if err := installExecutable(exe, stablePath); err != nil {
			return err
		}
		exe = stablePath
		fmt.Printf("installed stable executable: %s\n", exe)
	}
	alpacaPath, err := exec.LookPath("alpaca")
	if err != nil {
		return fmt.Errorf("alpaca executable not found in PATH: %w", err)
	}
	launchAgentsDir := filepath.Join(home, "Library", "LaunchAgents")
	if err := os.MkdirAll(launchAgentsDir, 0o750); err != nil {
		return err
	}
	plistPath := filepath.Join(launchAgentsDir, launchAgentLabel+".plist")
	logDir := filepath.Join(home, "Library", "Logs", "macswitcher")
	if err := os.MkdirAll(logDir, 0o750); err != nil {
		return err
	}
	plist := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key>
  <string>%s</string>
  <key>ProgramArguments</key>
  <array>
    <string>%s</string>
    <string>proxy</string>
    <string>run</string>
  </array>
  <key>RunAtLoad</key>
  <true/>
  <key>KeepAlive</key>
  <true/>
  <key>StandardOutPath</key>
  <string>%s</string>
  <key>StandardErrorPath</key>
  <string>%s</string>
  <key>EnvironmentVariables</key>
  <dict>
    <key>MACSWITCHER_ALPACA_BINARY</key>
    <string>%s</string>
  </dict>
</dict>
</plist>
`, launchAgentLabel, xmlEscape(exe), xmlEscape(filepath.Join(logDir, "proxy.out.log")), xmlEscape(filepath.Join(logDir, "proxy.err.log")), xmlEscape(alpacaPath))
	if err := os.WriteFile(plistPath, []byte(plist), 0o600); err != nil {
		return err
	}
	fmt.Printf("installed launch agent: %s\n", plistPath)
	fmt.Println("run: macswitcher service start")
	return nil
}

func isGoRunExecutable(path string) bool {
	return strings.Contains(path, string(filepath.Separator)+"go-build")
}

func installExecutable(source, destination string) error {
	content, err := os.ReadFile(source) // #nosec G304 -- source is the currently running executable's own os.Executable() path
	if err != nil {
		return fmt.Errorf("read executable %q: %w", source, err)
	}
	if err := os.MkdirAll(filepath.Dir(destination), 0o750); err != nil {
		return err
	}
	if err := os.WriteFile(destination, content, 0o700); err != nil { // #nosec G306,G703 -- must remain executable by the owner; destination is derived from a fixed ~/.local/bin/macswitcher path, not external input
		return fmt.Errorf("install executable %q: %w", destination, err)
	}
	return nil
}

func serviceUninstall() error {
	if err := serviceStop(); err != nil {
		fmt.Printf("warning: could not stop service before uninstall: %v\n", err)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	plistPath := filepath.Join(home, "Library", "LaunchAgents", launchAgentLabel+".plist")
	if err := os.Remove(plistPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	fmt.Printf("removed launch agent: %s\n", plistPath)
	return nil
}

func serviceStart() error {
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	plistPath := filepath.Join(home, "Library", "LaunchAgents", launchAgentLabel+".plist")
	if _, err := os.Stat(plistPath); err != nil {
		return fmt.Errorf("launch agent not found: %s (run service install first)", plistPath)
	}
	domain := serviceDomain()
	if serviceLoaded() {
		if err := runCommand("launchctl", "bootout", domain, plistPath); err != nil {
			return err
		}
	}
	if err := runCommand("launchctl", "bootstrap", domain, plistPath); err != nil {
		return err
	}
	if err := runCommand("launchctl", "enable", domain+"/"+launchAgentLabel); err != nil {
		return err
	}
	fmt.Println("launch agent started")
	return nil
}

func serviceStop() error {
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	plistPath := filepath.Join(home, "Library", "LaunchAgents", launchAgentLabel+".plist")
	if _, err := os.Stat(plistPath); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	if !serviceLoaded() {
		return nil
	}
	if err := runCommand("launchctl", "bootout", serviceDomain(), plistPath); err != nil {
		return err
	}
	fmt.Println("launch agent stopped")
	return nil
}

func serviceRestart() error {
	if err := serviceStop(); err != nil {
		fmt.Printf("warning: stop failed: %v\n", err)
	}
	if err := serviceStart(); err != nil {
		return err
	}
	fmt.Println("launch agent restarted")
	return nil
}

func serviceStatus() error {
	out, err := runCommandOutput("launchctl", "print", serviceDomain()+"/"+launchAgentLabel)
	if err != nil {
		fmt.Println("not loaded")
		return nil
	}

	fmt.Println(out)
	return nil
}

func serviceDomain() string {
	return "gui/" + fmt.Sprint(os.Getuid())
}

func serviceLoaded() bool {
	_, err := runCommandOutput("launchctl", "print", serviceDomain()+"/"+launchAgentLabel)
	return err == nil
}

func xmlEscape(s string) string {
	r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;", "'", "&apos;")
	return r.Replace(s)
}

func runCommand(name string, args ...string) error {
	cmd := exec.Command(name, args...) // #nosec G204 -- args are macswitcher-internal command definitions from trusted config/system calls, not raw user input
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("command failed: %s %s: %w", name, strings.Join(args, " "), err)
	}
	return nil
}

func runCommandList(parts []string) error {
	if len(parts) == 0 {
		return errors.New("empty command")
	}
	return runCommand(parts[0], parts[1:]...)
}

func runCommandOutput(name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...) // #nosec G204 -- args are macswitcher-internal command definitions from trusted config/system calls, not raw user input
	b, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("command failed: %s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(b)))
	}
	return strings.TrimSpace(string(b)), nil
}
