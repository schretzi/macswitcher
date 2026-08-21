package app

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

func updateZshProxy(proxyURL, noProxy string, enable bool) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	rcDir := filepath.Join(home, ".zsh", "rcs")
	if err := os.MkdirAll(rcDir, 0o755); err != nil {
		return err
	}
	rcPath := filepath.Join(rcDir, "proxy")

	host := ""
	port := ""
	if strings.TrimSpace(proxyURL) != "" {
		u, err := url.Parse(proxyURL)
		if err != nil {
			return fmt.Errorf("invalid proxy url %q: %w", proxyURL, err)
		}
		host = u.Hostname()
		port = u.Port()
	}
	if noProxy == "" {
		noProxy = "localhost,127.0.0.1,::1"
	}

	state := "off"
	if enable {
		state = "on"
	}

	content := strings.Join([]string{
		"# Proxy configuration values (managed by macswitcher)",
		fmt.Sprintf("export PROXY_STATE=%q", state),
		fmt.Sprintf("export PROXY_HOST=%q", host),
		fmt.Sprintf("export PROXY_PORT=%q", port),
		fmt.Sprintf("export PROXY_URL=%q", proxyURL),
		fmt.Sprintf("export PROXY_NO_PROXY=%q", noProxy),
		fmt.Sprintf("export http_proxy=%q", proxyURL),
		fmt.Sprintf("export https_proxy=%q", proxyURL),
		fmt.Sprintf("export HTTP_PROXY=%q", proxyURL),
		fmt.Sprintf("export HTTPS_PROXY=%q", proxyURL),
		fmt.Sprintf("export no_proxy=%q", noProxy),
		fmt.Sprintf("export NO_PROXY=%q", noProxy),
		"",
	}, "\n")

	return os.WriteFile(rcPath, []byte(content), 0o644)
}

func removeManagedZshBlock(content string) string {
	start := "# >>> macswitcher >>>"
	end := "# <<< macswitcher <<<"
	for {
		s := strings.Index(content, start)
		if s == -1 {
			break
		}
		e := strings.Index(content[s:], end)
		if e == -1 {
			content = content[:s]
			break
		}
		e += s + len(end)
		if e < len(content) && content[e] == '\n' {
			e++
		}
		content = content[:s] + content[e:]
	}
	return content
}

func updateDockerProxy(proxyURL, noProxy string, enable bool) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	dockerConfig := filepath.Join(home, ".docker", "config.json")
	if err := os.MkdirAll(filepath.Dir(dockerConfig), 0o755); err != nil {
		return err
	}
	obj := map[string]any{}
	if b, err := os.ReadFile(dockerConfig); err == nil {
		if len(strings.TrimSpace(string(b))) > 0 {
			if err := json.Unmarshal(b, &obj); err != nil {
				return fmt.Errorf("parse docker config: %w", err)
			}
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}

	proxiesAny, ok := obj["proxies"]
	var proxies map[string]any
	if ok {
		cast, ok := proxiesAny.(map[string]any)
		if !ok {
			return errors.New("docker config proxies field is not an object")
		}
		proxies = cast
	} else {
		proxies = map[string]any{}
		obj["proxies"] = proxies
	}

	if enable {
		proxies["default"] = map[string]any{
			"httpProxy":  proxyURL,
			"httpsProxy": proxyURL,
			"noProxy":    noProxy,
		}
	} else {
		delete(proxies, "default")
		if len(proxies) == 0 {
			delete(obj, "proxies")
		}
	}

	b, err := json.MarshalIndent(obj, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(dockerConfig, append(b, '\n'), 0o644)
}

func syncContextApplications(cfg Config, ctx SwitchContext) error {
	actions := []struct {
		name         string
		applications []string
	}{
		{name: "stop", applications: ctx.Apps.Stop},
		{name: "restart", applications: ctx.Apps.Restart},
		{name: "reload", applications: ctx.Apps.Reload},
		{name: "start", applications: ctx.Apps.Start},
	}
	for _, action := range actions {
		for _, name := range action.applications {
			name = strings.TrimSpace(name)
			if name == "" {
				continue
			}
			if err := runApplicationAction(name, action.name, cfg.Applications[name]); err != nil {
				return err
			}
		}
	}
	return nil
}

func runApplicationAction(name, action string, commands ApplicationCommands) error {
	command := applicationCommand(commands, action)
	if len(command) > 0 {
		if err := runCommandList(command); err != nil {
			return fmt.Errorf("application %q %s command failed: %w", name, action, err)
		}
		return nil
	}
	if action != "restart" && action != "reload" {
		return fmt.Errorf("application %q has no %s command", name, action)
	}
	if len(commands.Stop) == 0 || len(commands.Start) == 0 {
		return fmt.Errorf("application %q has no %s command or stop/start fallback", name, action)
	}
	if err := runCommandList(commands.Stop); err != nil {
		return fmt.Errorf("application %q stop fallback for %s failed: %w", name, action, err)
	}
	if err := runCommandList(commands.Start); err != nil {
		return fmt.Errorf("application %q start fallback for %s failed: %w", name, action, err)
	}
	return nil
}

func applicationCommand(commands ApplicationCommands, action string) []string {
	switch action {
	case "start":
		return commands.Start
	case "stop":
		return commands.Stop
	case "restart":
		return commands.Restart
	case "reload":
		return commands.Reload
	default:
		return nil
	}
}
