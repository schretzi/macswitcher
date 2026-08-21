package app

import (
	"errors"
	"flag"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

func detectAuth(cfgPath string, args []string) error {
	fs := flag.NewFlagSet("detect-auth", flag.ContinueOnError)
	proxy := fs.String("proxy", "", "upstream proxy host:port")
	target := fs.String("url", "https://example.com", "target URL for probe")
	if err := fs.Parse(args); err != nil {
		return err
	}

	proxyAddr := strings.TrimSpace(*proxy)
	if proxyAddr == "" {
		cfg, err := loadConfig(cfgPath)
		if err != nil {
			return err
		}
		ctx := cfg.Contexts[cfg.CurrentContext]
		if ctx.ForwarderProxy == nil {
			return errors.New("active context has no forwarder_proxy configured")
		}
		if err := validateForwarderProxy(*ctx.ForwarderProxy); err != nil {
			return err
		}
		proxyAddr = fmt.Sprintf("%s:%d", ctx.ForwarderProxy.ProxyServer, ctx.ForwarderProxy.Port)
	}

	methods, statusCode, raw, err := probeProxyAuth(proxyAddr, *target)
	if err != nil {
		return err
	}
	fmt.Printf("proxy: %s\n", proxyAddr)
	fmt.Printf("target: %s\n", *target)
	fmt.Printf("http_status: %d\n", statusCode)
	if len(methods) == 0 {
		fmt.Println("proxy_auth_methods: none detected")
		fmt.Println("recommendation: no auth challenge observed; verify proxy path or force a protected URL")
		fmt.Printf("raw_headers:\n%s\n", raw)
		return nil
	}
	fmt.Printf("proxy_auth_methods: %s\n", strings.Join(methods, ", "))
	fmt.Printf("recommendation: %s\n", recommendRuntime(methods))
	return nil
}

func probeProxyAuth(proxyAddr, targetURL string) ([]string, int, string, error) {
	proxyURL := proxyAddr
	if !strings.Contains(proxyURL, "://") {
		proxyURL = "http://" + proxyURL
	}
	cmd := exec.Command("curl", "-sS", "-o", "/dev/null", "-D", "-", "-x", proxyURL, "--max-time", "12", targetURL)
	b, err := cmd.CombinedOutput()
	raw := string(b)
	statusCode := parseHTTPStatus(raw)
	methods := parseProxyAuthenticateHeaders(raw)
	if err != nil {
		if statusCode == 407 || len(methods) > 0 {
			return methods, statusCode, raw, nil
		}
		return nil, 0, "", fmt.Errorf("curl probe failed: %w\n%s", err, raw)
	}
	return methods, statusCode, raw, nil
}

func parseHTTPStatus(headers string) int {
	for _, line := range strings.Split(headers, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(strings.ToUpper(line), "HTTP/") {
			parts := strings.Fields(line)
			if len(parts) >= 2 {
				if code, err := strconv.Atoi(parts[1]); err == nil {
					return code
				}
			}
		}
	}
	return 0
}

func parseProxyAuthenticateHeaders(headers string) []string {
	seen := map[string]bool{}
	methods := make([]string, 0)
	for _, line := range strings.Split(headers, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(strings.ToLower(line), "proxy-authenticate:") {
			continue
		}
		v := strings.TrimSpace(strings.TrimPrefix(line, "Proxy-Authenticate:"))
		v = strings.TrimSpace(strings.TrimPrefix(v, "proxy-authenticate:"))
		if v == "" {
			continue
		}
		parts := strings.Fields(v)
		if len(parts) == 0 {
			continue
		}
		m := strings.ToUpper(parts[0])
		if !seen[m] {
			seen[m] = true
			methods = append(methods, m)
		}
	}
	return methods
}

func recommendRuntime(methods []string) string {
	has := func(target string) bool {
		for _, m := range methods {
			if strings.EqualFold(m, target) {
				return true
			}
		}
		return false
	}
	if has("NTLM") {
		if has("NEGOTIATE") || has("KERBEROS") {
			return "Negotiate and NTLM detected: try cntlm first, then evaluate Kerberos-native client path if policy requires it"
		}
		return "NTLM detected: cntlm is usually the most reliable local bridge for CLI/Docker tooling"
	}
	if has("NEGOTIATE") || has("KERBEROS") {
		return "Kerberos/Negotiate detected: prefer Kerberos-native client path; cntlm may not satisfy strict Kerberos-only policy"
	}
	if has("BASIC") {
		return "Basic auth detected: cntlm should work, and direct client proxy config is also an option"
	}
	return "Auth method detected but uncommon; test cntlm first, then evaluate Kerberos-native client path if authentication still fails"
}

func runProxy(cfgPath string) error {
	cfg, err := loadConfig(cfgPath)
	if err != nil {
		return err
	}
	alpaca := cfg.Alpaca
	if !cfg.Alpaca.Enabled {
		return errors.New("alpaca is disabled globally")
	}
	if ctx, ok := cfg.Contexts[cfg.CurrentContext]; ok && ctx.Alpaca != nil {
		if !ctx.Alpaca.Enabled {
			return errors.New("alpaca is disabled for the active context")
		}
		if len(ctx.Alpaca.Command) > 0 {
			alpaca.Command = ctx.Alpaca.Command
		}
	}
	cmdArgs, err := buildProxyCommand(cfg, alpaca)
	if err != nil {
		return err
	}
	if len(cmdArgs) == 0 {
		return errors.New("alpaca command is empty")
	}
	cmd := exec.Command(cmdArgs[0], cmdArgs[1:]...)
	if ctx, ok := cfg.Contexts[cfg.CurrentContext]; ok && strings.TrimSpace(ctx.Kerberos.TicketFile) != "" {
		ticketFile := strings.TrimSpace(ctx.Kerberos.TicketFile)
		if strings.HasPrefix(ticketFile, "~/") {
			if home, err := os.UserHomeDir(); err == nil {
				ticketFile = filepath.Join(home, strings.TrimPrefix(ticketFile, "~/"))
			}
		}
		cmd.Env = append(os.Environ(), "KRB5CCNAME="+ticketFile)
	}
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Stdin = os.Stdin
	fmt.Printf("running proxy for context %q: %s\n", cfg.CurrentContext, redactPasswordFromCommand(cmdArgs))
	return cmd.Run()
}

func buildProxyCommand(cfg Config, alpaca AlpacaConfig) ([]string, error) {
	if len(alpaca.Command) == 0 {
		return nil, errors.New("alpaca command is empty")
	}
	command := append([]string(nil), alpaca.Command...)
	if command[0] == "alpaca" {
		if binary := strings.TrimSpace(os.Getenv("MACSWITCHER_ALPACA_BINARY")); binary != "" {
			command[0] = binary
		}
	}
	var forwarderProxy *ForwarderProxyConfig
	if ctx, ok := cfg.Contexts[cfg.CurrentContext]; ok {
		forwarderProxy = ctx.ForwarderProxy
	}
	forwarder := ForwarderProxyConfig{}
	password := ""
	var err error
	if forwarderProxy != nil {
		forwarder = *forwarderProxy
		if forwarder.PasswordKeychainAccount == "" {
			forwarder.PasswordKeychainAccount = forwarder.Username
		}
		if err := validateForwarderProxy(forwarder); err != nil {
			return nil, err
		}
		password, err = keychainPasswordGet(forwarder.PasswordKeychainService, forwarder.PasswordKeychainAccount)
		if err != nil {
			return nil, err
		}
	}
	cntlmConfPath := ""
	if commandUsesToken(command, "{{cntlm_conf}}") {
		if forwarderProxy == nil {
			return nil, errors.New("{{cntlm_conf}} requires an active forwarder_proxy")
		}
		cntlmConfPath, err = generateCntlmConfig(cfg, password)
		if err != nil {
			return nil, err
		}
	}
	upstreamURL := ""
	if commandUsesToken(command, "{{upstream_url}}") {
		if forwarderProxy == nil {
			return nil, errors.New("{{upstream_url}} requires an active forwarder_proxy")
		}
		upstreamURL, err = buildForwarderUpstreamURL(forwarder, password)
		if err != nil {
			return nil, err
		}
	}
	authAllowlist := strings.Join(forwarder.AuthAllowlist, ",")
	ticketFile := ""
	upstreamProxy := ""
	if ctx, ok := cfg.Contexts[cfg.CurrentContext]; ok {
		ticketFile = ctx.Kerberos.TicketFile
		upstreamProxy = ctx.Kerberos.UpstreamProxy
	}
	replacements := map[string]string{
		"{{local_host}}":     cfg.LocalProxy.Host,
		"{{local_port}}":     strconv.Itoa(cfg.LocalProxy.Port),
		"{{cntlm_conf}}":     cntlmConfPath,
		"{{proxy_server}}":   forwarder.ProxyServer,
		"{{proxy_port}}":     strconv.Itoa(forwarder.Port),
		"{{username}}":       forwarder.Username,
		"{{password}}":       password,
		"{{pac_file}}":       forwarder.PacFile,
		"{{upstream_url}}":   upstreamURL,
		"{{auth_allowlist}}": authAllowlist,
		"{{ticket_file}}":    ticketFile,
		"{{upstream_proxy}}": upstreamProxy,
	}
	out := make([]string, 0, len(command))
	for i := 0; i < len(command); i++ {
		arg := command[i]
		if arg == "-C" && i+1 < len(command) && command[i+1] == "{{pac_file}}" && forwarder.PacFile == "" {
			i++
			continue
		}
		expanded := arg
		for k, v := range replacements {
			expanded = strings.ReplaceAll(expanded, k, v)
		}
		if strings.TrimSpace(expanded) == "" {
			continue
		}
		out = append(out, expanded)
	}
	return out, nil
}

func generateCntlmConfig(cfg Config, password string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	generatedDir := filepath.Join(home, ".config", "macswitcher", "generated")
	if err := os.MkdirAll(generatedDir, 0o700); err != nil {
		return "", err
	}
	contextName := cfg.CurrentContext
	if contextName == "" {
		contextName = "default"
	}
	confPath := filepath.Join(generatedDir, "cntlm-"+contextName+".conf")
	ctx := cfg.Contexts[cfg.CurrentContext]
	if ctx.ForwarderProxy == nil {
		return "", errors.New("active context has no forwarder_proxy configured")
	}
	proxy := *ctx.ForwarderProxy
	domain, user := splitDomainAndUser(proxy.Username)
	if user == "" {
		user = proxy.Username
	}
	lines := []string{
		"# Generated by macswitcher. Permissions are 0600.",
		"Listen " + fmt.Sprintf("%s:%d", cfg.LocalProxy.Host, cfg.LocalProxy.Port),
		"Proxy " + fmt.Sprintf("%s:%d", proxy.ProxyServer, proxy.Port),
		"Username " + user,
		"Auth NTLMv2",
		"Password " + password,
	}
	if domain != "" {
		lines = append(lines, "Domain "+domain)
	}
	if len(cfg.LocalProxy.NoProxy) > 0 {
		lines = append(lines, "NoProxy "+strings.Join(cfg.LocalProxy.NoProxy, ","))
	}
	content := strings.Join(lines, "\n") + "\n"
	if err := os.WriteFile(confPath, []byte(content), 0o600); err != nil {
		return "", err
	}
	return confPath, nil
}

func commandUsesToken(parts []string, token string) bool {
	for _, part := range parts {
		if strings.Contains(part, token) {
			return true
		}
	}
	return false
}

func splitDomainAndUser(raw string) (string, string) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "", ""
	}
	if parts := strings.SplitN(trimmed, "\\", 2); len(parts) == 2 {
		return parts[0], parts[1]
	}
	if parts := strings.SplitN(trimmed, "/", 2); len(parts) == 2 {
		return parts[0], parts[1]
	}
	return "", trimmed
}

func validateForwarderProxy(ep ForwarderProxyConfig) error {
	if strings.TrimSpace(ep.ProxyServer) == "" {
		return errors.New("forwarder_proxy.proxy_server is required")
	}
	if ep.Port <= 0 {
		return errors.New("forwarder_proxy.port must be > 0")
	}
	if strings.TrimSpace(ep.Username) == "" {
		return errors.New("forwarder_proxy.username is required")
	}
	if strings.TrimSpace(ep.PasswordKeychainService) == "" {
		return errors.New("forwarder_proxy.password_keychain_service is required")
	}
	return nil
}

func buildForwarderUpstreamURL(ep ForwarderProxyConfig, password string) (string, error) {
	if err := validateForwarderProxy(ep); err != nil {
		return "", err
	}
	u := &url.URL{
		Scheme: "http",
		Host:   fmt.Sprintf("%s:%d", ep.ProxyServer, ep.Port),
		User:   url.UserPassword(ep.Username, password),
	}
	return u.String(), nil
}

func keychainPasswordSet(cfgPath string) error {
	cfg, err := loadConfig(cfgPath)
	if err != nil {
		return err
	}
	ctx := cfg.Contexts[cfg.CurrentContext]
	if ctx.ForwarderProxy == nil {
		return errors.New("active context has no forwarder_proxy configured")
	}
	proxy := *ctx.ForwarderProxy
	if err := validateForwarderProxy(proxy); err != nil {
		return err
	}
	service := proxy.PasswordKeychainService
	account := proxy.PasswordKeychainAccount
	if strings.TrimSpace(account) == "" {
		account = proxy.Username
	}
	fmt.Printf("setting keychain password for service=%q account=%q\n", service, account)
	fmt.Println("a macOS keychain prompt may appear")
	cmd := exec.Command("security", "add-generic-password", "-U", "-s", service, "-a", account, "-w")
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("failed to set keychain password: %w", err)
	}
	return nil
}

func keychainPasswordGet(service, account string) (string, error) {
	args := []string{"find-generic-password", "-s", service, "-w"}
	if strings.TrimSpace(account) != "" {
		args = append(args, "-a", account)
	}
	cmd := exec.Command("security", args...)
	b, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("read keychain password failed for service=%q account=%q: %w", service, account, err)
	}
	password := strings.TrimSpace(string(b))
	if password == "" {
		return "", fmt.Errorf("empty keychain password for service=%q account=%q", service, account)
	}
	return password, nil
}

func redactPasswordFromCommand(args []string) string {
	joined := strings.Join(args, " ")
	re := regexp.MustCompile(`(?i)(--password\s+)(\S+)`)
	joined = re.ReplaceAllString(joined, `${1}********`)
	reEnv := regexp.MustCompile(`(?i)((BASIC_CREDENTIALS|NTLM_CREDENTIALS)=)(\S+)`)
	joined = reEnv.ReplaceAllString(joined, `${1}********`)
	reURL := regexp.MustCompile(`://([^:\s]+):([^@\s]+)@`)
	joined = reURL.ReplaceAllString(joined, `://$1:********@`)
	return joined
}
