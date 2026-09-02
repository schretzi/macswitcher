package app

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"

	"github.com/schretzi/macswitcher/internal/logfile"
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
	ctx, cancel := context.WithTimeout(context.Background(), commandTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "curl", "-sS", "-o", "/dev/null", "-D", "-", "-x", proxyURL, "--max-time", "12", targetURL) // #nosec G204 -- fixed "curl" binary; args are constructed proxy/target URLs, not passed through a shell
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
	for line := range strings.SplitSeq(headers, "\n") {
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
	for line := range strings.SplitSeq(headers, "\n") {
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
	// alpaca tries Negotiate, then NTLM, then Basic, and needs no help
	// choosing: it is told what is available and picks the strongest the
	// proxy accepts. So these are notes on what to make available, not on
	// which tool to reach for.
	if has("NEGOTIATE") || has("KERBEROS") {
		if has("NTLM") {
			return "Negotiate and NTLM offered: a Kerberos ticket is used when present, with Basic as the fallback"
		}
		return "Kerberos/Negotiate offered: keep a ticket alive (kerberoskeepalive), since Basic may be refused"
	}
	if has("NTLM") {
		return "NTLM offered: alpaca falls back to Basic, which this proxy may refuse - verify before relying on it"
	}
	if has("BASIC") {
		return "Basic offered: the Keychain password alone is enough, no ticket needed"
	}
	return "Auth method detected but uncommon; check whether the proxy accepts Basic, which is alpaca's last resort"
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
	// No timeout: this is the proxy itself and runs until launchd stops it.
	cmd := exec.CommandContext(context.Background(), cmdArgs[0], cmdArgs[1:]...) // #nosec G204 -- cmdArgs come from the operator-controlled config file (Alpaca command), not untrusted input
	env, err := proxyEnv(cfg)
	if err != nil {
		return err
	}
	cmd.Env = env
	// alpaca is chatty and runs for as long as the session does, so its output
	// goes to macswitcher's own log rather than to launchd's StandardOutPath.
	// newsyslog rotates that log by renaming it, and a plain inherited fd
	// would go on filling the archive while the live log stayed empty -
	// logfile.Writer re-stats and reopens instead. See the
	// macos-launchd-services skill.
	logPath, err := proxyLogPath()
	if err != nil {
		return err
	}
	logWriter, err := logfile.Open(logPath)
	if err != nil {
		return err
	}
	defer logWriter.Close()

	cmd.Stdout = logWriter
	cmd.Stderr = logWriter
	cmd.Stdin = os.Stdin

	banner := fmt.Sprintf("running proxy for context %q: %s\n", cfg.CurrentContext, redactPasswordFromCommand(cmdArgs))
	fmt.Print(banner)
	if _, err := io.WriteString(logWriter, banner); err != nil {
		return fmt.Errorf("writing to %s: %w", logPath, err)
	}
	return cmd.Run()
}

// proxyEnv builds alpaca's environment, adding the Basic credentials that are
// its last-resort authentication method.
//
// alpaca tries Negotiate, then NTLM, then Basic, and drops any method it has
// no credentials for. Its macOS GSS integration reads the default ccache, but
// KerberosKeepAlive deliberately maintains a named FILE cache. Point GSS at
// that file so a valid KKA ticket is actually usable for Negotiate.
//
// When there is no valid ticket the chain used to be empty and every request
// through the proxy failed. That is not hypothetical: it is what a KDC that
// cannot be discovered leaves behind, and it took the whole proxy down with it.
//
// Passing the Keychain password as BASIC_CREDENTIALS gives the chain a rung to
// fall back to. Basic is slower (it authenticates per request) and weaker, so
// it is deliberately last - alpaca still prefers the ticket whenever one
// exists, and only reaches this when it does not.
//
// The password goes in the environment rather than in argv because argv is
// world-readable through ps, and an environment is not: on macOS `ps -E` shows
// another user's environment only to root. This is also the interface alpaca
// documents for exactly that reason.
func proxyEnv(cfg Config) ([]string, error) {
	env := os.Environ()
	ctx, ok := cfg.Contexts[cfg.CurrentContext]
	if !ok || !isForwardProxyMode(ctx.ProxyMode) || ctx.ForwarderProxy == nil {
		return env, nil
	}
	if ccachePath := kerberosCcacheFromKeepAlive(); ccachePath != "" {
		// ccache_path is a filesystem path in KerberosKeepAlive's config, not
		// a KRB5CCNAME URI. Put this after os.Environ so an interactive shell's
		// unrelated cache cannot override the one KKA refreshes.
		env = replaceEnv(env, "KRB5CCNAME", "FILE:"+ccachePath)
	}
	fp := *ctx.ForwarderProxy
	if fp.PasswordKeychainAccount == "" {
		fp.PasswordKeychainAccount = fp.Username
	}
	if strings.TrimSpace(fp.Username) == "" || strings.TrimSpace(fp.PasswordKeychainService) == "" {
		return env, nil
	}
	password, err := keychainPasswordGet(fp.PasswordKeychainService, fp.PasswordKeychainAccount)
	if err != nil {
		return nil, fmt.Errorf("read forwarder proxy password from Keychain: %w", err)
	}
	if password == "" {
		return env, nil
	}
	// alpaca base64-encodes this itself; it wants the raw "user:password".
	return append(env, "BASIC_CREDENTIALS="+fp.Username+":"+password), nil
}

func replaceEnv(env []string, name, value string) []string {
	prefix := name + "="
	out := make([]string, 0, len(env)+1)
	for _, entry := range env {
		if !strings.HasPrefix(entry, prefix) {
			out = append(out, entry)
		}
	}
	return append(out, prefix+value)
}

// proxyLogPath is ~/Library/Logs/macswitcher.log - flat, named after the
// binary, matching every other job.
func proxyLogPath() (string, error) {
	return launchAgentService().LogPath()
}

func buildProxyCommand(cfg Config, alpaca AlpacaConfig) ([]string, error) { //nolint:gocyclo // TODO: split this up. Left as-is for now because it drives live network/VPN/proxy switching and a refactor needs its own test pass.
	if len(alpaca.Command) == 0 {
		return nil, errors.New("alpaca command is empty")
	}
	command := append([]string(nil), alpaca.Command...)
	if command[0] == appAlpaca {
		if binary := strings.TrimSpace(os.Getenv("MACSWITCHER_ALPACA_BINARY")); binary != "" {
			command[0] = binary
		}
	}
	var forwarderProxy *ForwarderProxyConfig
	if ctx, ok := cfg.Contexts[cfg.CurrentContext]; ok && isForwardProxyMode(ctx.ProxyMode) {
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
	// The filter proxy, in direct contexts: alpaca's only way to name an
	// upstream is a PAC file, so enabling filter_proxy means generating one
	// and letting it fill the same {{pac_file}} placeholder a forward context
	// fills with the corporate PAC. The two can never collide - one is
	// direct-only, the other forward-only.
	pacFile := forwarder.PacFile
	if ctx, ok := cfg.Contexts[cfg.CurrentContext]; ok && filterProxyAppliesTo(cfg, ctx) {
		generated, err := writeFilterPAC(cfg)
		if err != nil {
			return nil, fmt.Errorf("generate filter proxy PAC: %w", err)
		}
		pacFile = "file://" + generated
	}

	authAllowlist := strings.Join(forwarder.AuthAllowlist, ",")
	upstreamProxy := ""
	if strings.TrimSpace(forwarder.ProxyServer) != "" {
		upstreamProxy = fmt.Sprintf("%s:%d", forwarder.ProxyServer, forwarder.Port)
	}
	replacements := map[string]string{
		placeholderLocalHost: cfg.LocalProxy.Host,
		placeholderLocalPort: strconv.Itoa(cfg.LocalProxy.Port),
		"{{proxy_server}}":   forwarder.ProxyServer,
		"{{proxy_port}}":     strconv.Itoa(forwarder.Port),
		"{{username}}":       forwarder.Username,
		"{{password}}":       password,
		placeholderPACFile:   pacFile,
		"{{upstream_url}}":   upstreamURL,
		"{{auth_allowlist}}": authAllowlist,
		"{{upstream_proxy}}": upstreamProxy,
	}
	out := make([]string, 0, len(command))
	for i := 0; i < len(command); i++ {
		arg := command[i]
		if arg == "-C" && i+1 < len(command) && command[i+1] == placeholderPACFile && pacFile == "" {
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

func commandUsesToken(parts []string, token string) bool {
	for _, part := range parts {
		if strings.Contains(part, token) {
			return true
		}
	}
	return false
}

func validateForwarderProxy(ep ForwarderProxyConfig) error {
	if strings.TrimSpace(ep.ProxyServer) == "" {
		return errors.New("forwarder_proxy.proxy_server is required")
	}
	if ep.Port <= 0 {
		return errors.New("forwarder_proxy.port must be > 0")
	}
	// Username and a Keychain-backed password are now always required: they
	// are what alpaca's Basic fallback needs, and that fallback is the only
	// thing standing between a missing Kerberos ticket and a dead proxy.
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
	if strings.TrimSpace(proxy.PasswordKeychainService) == "" {
		return errors.New("forwarder_proxy.password_keychain_service is required")
	}
	service := proxy.PasswordKeychainService
	account := proxy.PasswordKeychainAccount
	if strings.TrimSpace(account) == "" {
		account = proxy.Username
	}
	fmt.Printf("setting keychain password for service=%q account=%q\n", service, account)
	fmt.Println("a macOS keychain prompt may appear")
	// Not `ctx`: that name is already the macswitcher context in this scope.
	cmdCtx, cancel := context.WithTimeout(context.Background(), keychainTimeout)
	defer cancel()
	cmd := exec.CommandContext(cmdCtx, "security", "add-generic-password", "-U", "-s", service, "-a", account, "-w") // #nosec G204 -- fixed macOS "security" binary; service/account come from trusted config
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("failed to set keychain password: %w", err)
	}
	return nil
}

// keychainPasswordGet is a variable so tests can supply a password without a
// real Keychain, which is neither present nor unlockable in CI.
var keychainPasswordGet = func(service, account string) (string, error) {
	args := []string{"find-generic-password", "-s", service, "-w"}
	if strings.TrimSpace(account) != "" {
		args = append(args, "-a", account)
	}
	ctx, cancel := context.WithTimeout(context.Background(), keychainTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "security", args...) // #nosec G204 -- fixed macOS "security" binary; args come from trusted config
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
