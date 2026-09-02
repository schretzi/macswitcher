package app

import (
	"errors"
	"flag"
	"fmt"
	"strings"
)

func switchContext(cfgPath string, args []string) error {
	fs := flag.NewFlagSet("switch", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("usage: macswitcher switch <context>")
	}
	selected := fs.Arg(0)
	cfg, err := loadConfig(cfgPath)
	if err != nil {
		return err
	}
	ctx, ok := cfg.Contexts[selected]
	if !ok {
		return fmt.Errorf("context %q not found", selected)
	}
	previous := cfg.CurrentContext
	if err := applyContext(cfgPath, cfg, ctx, selected); err != nil {
		rollbackContext(cfgPath, cfg, previous, selected)
		return err
	}
	fmt.Printf("switched context to %s\n", selected)
	return nil
}

// applyContext puts the machine into one context. Split out of switchContext
// so a failed switch can be undone by applying the previous context with the
// same code path - see rollbackContext.
//
// Order matters, and not in the obvious way:
//
//   - The runtime state is written first because setLocalProxy, unsetLocalProxy
//     and the proxy service all read the current context back off disk to
//     decide whether the filtering proxy is in the path.
//   - Applications run *before* checkDNSResolution. A forward context reaches
//     its corporate proxy's name only through the VPN, and the VPN is one of
//     these applications: checking DNS first made such a context impossible to
//     switch into from a machine with no tunnel up. It also means the filtering
//     proxy is already listening by the time the system proxy is pointed at it.
//   - The proxy is changed last, so anything that fails above leaves the
//     machine on the proxy settings it already had rather than half-way onto
//     new ones.
func applyContext(cfgPath string, cfg Config, ctx SwitchContext, name string) error {
	cfg.CurrentContext = name
	if err := saveRuntimeState(cfgPath, ConfigState{CurrentContext: name}); err != nil {
		return err
	}

	if strings.TrimSpace(ctx.MacOSNetworkLocation) != "" {
		if err := runCommand("scselect", ctx.MacOSNetworkLocation); err != nil {
			fmt.Printf("warning: could not switch macOS network location: %v\n", err)
		}
	}
	if len(ctx.Upstreams) > 0 {
		if err := syncAdGuardUpstreams(cfg, ctx.Upstreams, name); err != nil {
			return err
		}
	}
	if err := applyLocalResolverDNS(cfg); err != nil {
		return err
	}
	flushDNSCache()
	if err := syncContextApplications(cfg, ctx); err != nil {
		return err
	}
	if err := checkDNSResolution(ctx); err != nil {
		return fmt.Errorf("%w\nhint: DNS is not resolving after the switch; fix DNS (check AdGuard Home, VPN, network location) and rerun `macswitcher switch %s`", err, name)
	}
	if strings.EqualFold(ctx.ProxyMode, ProxyModeOff) { //nolint:nestif // TODO: split this up. Left as-is for now because it drives live network/VPN/proxy switching and a refactor needs its own test pass.
		if err := unsetLocalProxy(cfgPath); err != nil {
			return err
		}
		if err := serviceStop(); err != nil {
			fmt.Printf("warning: could not stop proxy service: %v\n", err)
		}
	} else {
		if err := serviceRestart(); err != nil {
			fmt.Printf("warning: could not restart proxy service automatically: %v\n", err)
		}
		if err := setLocalProxy(cfgPath); err != nil {
			return err
		}
	}
	return nil
}

// rollbackContext puts back the context that was active before a failed
// switch. Best effort, and loud about it either way.
//
// A switch mutates the machine in steps, so a failure part-way through leaves
// it in a state that matches neither context: resolvers pointed at a network
// that is not reachable, a VPN up with no proxy to use it, a filtering proxy
// stopped by the new context and not started by anything. That state is worse
// than either end of the switch, and it is not obvious from the error which
// half of it happened - so undo it rather than leave the operator to guess.
func rollbackContext(cfgPath string, cfg Config, previous, failed string) {
	if strings.TrimSpace(previous) == "" || previous == failed {
		fmt.Printf("warning: the switch to %q failed part-way through and there is no different context to fall back to; the machine may be in a mixed state\n", failed)
		return
	}
	prev, ok := cfg.Contexts[previous]
	if !ok {
		fmt.Printf("warning: the switch to %q failed part-way through and the previous context %q no longer exists; the machine may be in a mixed state\n", failed, previous)
		return
	}
	fmt.Printf("the switch to %q failed part-way through; rolling back to %q\n", failed, previous)
	if err := applyContext(cfgPath, cfg, prev, previous); err != nil {
		fmt.Printf("warning: rolling back to %q failed as well: %v\n", previous, err)
		fmt.Printf("warning: the machine is in a mixed state; fix the cause and rerun `macswitcher switch %s`\n", previous)
		return
	}
	fmt.Printf("rolled back to %s\n", previous)
}

// syncAdGuardUpstreams points AdGuard Home at the upstream resolvers this
// context wants. Skipped entirely unless adguard.upstreams_file is set.
//
// Failing here aborts the switch. It used to be a warning, on the grounds
// that unbound was still the resolver the machine pointed at - that is no
// longer true, AdGuard Home is the only one. A failed write leaves it
// forwarding to the *previous* network's resolvers, and checkDNSResolution
// will not catch that: public names still resolve through the old upstreams,
// so the switch looks like it worked and only the intranet is gone.
func syncAdGuardUpstreams(cfg Config, forwarders []string, selected string) error {
	if strings.TrimSpace(cfg.AdGuard.UpstreamsFile) == "" {
		return nil
	}
	if err := writeAdGuardUpstreams(cfg, forwarders, selected); err != nil {
		return fmt.Errorf("write AdGuard Home upstreams: %w", err)
	}
	restartAdGuardIfConfigured(cfg)
	return nil
}

func status(cfgPath string) error {
	cfg, err := loadConfig(cfgPath)
	if err != nil {
		return err
	}
	proxyURL, noProxy := proxyEnvValues(cfg)
	ctx := cfg.Contexts[cfg.CurrentContext]
	fmt.Printf("config: %s\n", cfgPath)
	fmt.Printf("current_context: %s\n", cfg.CurrentContext)
	fmt.Printf("macos_network_location: %s\n", ctx.MacOSNetworkLocation)
	fmt.Printf("proxy_mode: %s\n", ctx.ProxyMode)
	fmt.Printf("local_proxy: %s\n", proxyURL)
	fmt.Printf("filter_proxy: %s\n", filterProxyStatusLine(cfg, ctx))
	fmt.Printf("local_resolver: %s\n", cfg.DNS.LocalResolver)
	fmt.Printf("adguard_upstreams_file: %s\n", cfg.AdGuard.UpstreamsFile)
	fmt.Printf("no_proxy: %s\n", noProxy)
	fmt.Printf("network_services configured: %d (0 means auto-detect)\n", len(cfg.NetworkServices))
	fmt.Println("service:")
	return serviceStatus()
}

func configValidate(cfgPath string) error { //nolint:gocyclo // TODO: split this up. Left as-is for now because it drives live network/VPN/proxy switching and a refactor needs its own test pass.
	cfg, err := loadConfig(cfgPath)
	if err != nil {
		return err
	}
	critical := make([]string, 0)
	warnings := make([]string, 0)
	daemonWarnings, err := daemonsConfigWarnings(cfgPath)
	if err != nil {
		warnings = append(warnings, fmt.Sprintf("could not check daemons: block for typos: %v", err))
	} else {
		warnings = append(warnings, daemonWarnings...)
	}
	for name, ctx := range cfg.Contexts {
		if isEmptyContext(ctx) {
			continue
		}
		if strings.TrimSpace(ctx.ProxyMode) == "" {
			warnings = append(warnings, fmt.Sprintf("contexts.%s.proxy_mode is empty (recommended: off|direct|forward)", name))
		} else if !isValidProxyMode(ctx.ProxyMode) {
			critical = append(critical, fmt.Sprintf("contexts.%s.proxy_mode %q is invalid (must be off, direct, or forward)", name, ctx.ProxyMode))
		}
		if isForwardProxyMode(ctx.ProxyMode) && ctx.ForwarderProxy == nil {
			critical = append(critical, fmt.Sprintf("contexts.%s.proxy_mode is forward but forwarder_proxy is not configured", name))
		}
		if ctx.ForwarderProxy != nil { //nolint:nestif // TODO: split this up. Left as-is for now because it drives live network/VPN/proxy switching and a refactor needs its own test pass.
			if !isForwardProxyMode(ctx.ProxyMode) {
				warnings = append(warnings, fmt.Sprintf("contexts.%s.forwarder_proxy is configured but proxy_mode is %q; it is only used when proxy_mode is forward", name, ctx.ProxyMode))
			}
			proxy := *ctx.ForwarderProxy
			if err := validateForwarderProxy(proxy); err != nil {
				critical = append(critical, fmt.Sprintf("contexts.%s.forwarder_proxy: %v", name, err))
			}
			if strings.TrimSpace(proxy.TicketFile) == "" && strings.TrimSpace(proxy.PasswordKeychainAccount) == "" {
				warnings = append(warnings, fmt.Sprintf("contexts.%s.forwarder_proxy.password_keychain_account is empty; runtime will fallback to username", name))
			}
			if len(proxy.AuthAllowlist) == 0 {
				warnings = append(warnings, fmt.Sprintf("contexts.%s.forwarder_proxy.auth_allowlist is empty; credentials may be sent to any PAC-selected proxy host", name))
			}
			for _, suffix := range proxy.AuthAllowlist {
				if strings.TrimSpace(suffix) == "*" {
					warnings = append(warnings, fmt.Sprintf("contexts.%s.forwarder_proxy.auth_allowlist contains '*', which is fully permissive", name))
				}
			}
			if strings.HasPrefix(strings.ToLower(strings.TrimSpace(proxy.PacFile)), "http://") {
				warnings = append(warnings, fmt.Sprintf("contexts.%s.forwarder_proxy.pac_file uses HTTP; keep auth_allowlist strict to reduce PAC tampering impact", name))
			}
		}
		if len(ctx.Upstreams) == 0 {
			warnings = append(warnings, fmt.Sprintf("contexts.%s.upstreams is empty", name))
		}
		// The wiring this replaced: a context that names its own PAC through
		// alpaca.command still works, but filter_proxy will not fill the
		// placeholder it never sees - so the two silently disagree about
		// which PAC alpaca ends up with.
		if cfg.FilterProxy.Enabled && ctx.Alpaca != nil && commandNamesPAC(ctx.Alpaca.Command) {
			warnings = append(warnings, fmt.Sprintf(
				"contexts.%s.alpaca.command names its own PAC (-C) while filter_proxy is enabled; "+
					"drop the override and let filter_proxy generate it", name,
			))
		}
		validateApplicationReferences(cfg, name, ctx.Apps, &warnings, &critical)
	}

	ctx := cfg.Contexts[cfg.CurrentContext]
	// Checked against the port, not against the config: a filter_proxy block
	// that describes a proxy nobody is running is exactly the state the
	// fail-open PAC hides.
	if filterProxyAppliesTo(cfg, ctx) && !filterProxyListening(cfg) {
		warnings = append(warnings, fmt.Sprintf(
			"filter_proxy is in the path for context %q but nothing is listening on %s%s",
			cfg.CurrentContext, filterProxyAddr(cfg),
			map[bool]string{true: " - traffic goes out unfiltered (fail_open)", false: " - requests will fail"}[cfg.FilterProxy.FailOpen],
		))
	}
	if isForwardProxyMode(ctx.ProxyMode) && ctx.ForwarderProxy != nil && strings.TrimSpace(ctx.ForwarderProxy.TicketFile) == "" {
		account := ctx.ForwarderProxy.PasswordKeychainAccount
		if strings.TrimSpace(account) == "" {
			account = ctx.ForwarderProxy.Username
		}
		if _, err := keychainPasswordGet(ctx.ForwarderProxy.PasswordKeychainService, account); err != nil {
			warnings = append(warnings, "no readable keychain password found for the active forwarder proxy; run macswitcher proxy password-set")
		}
	}

	fmt.Printf("config: %s\n", cfgPath)
	if len(critical) == 0 && len(warnings) == 0 {
		fmt.Println("validation: OK")
		return nil
	}
	if len(critical) > 0 {
		fmt.Println("critical:")
		for _, c := range critical {
			fmt.Printf("- %s\n", c)
		}
	}
	if len(warnings) > 0 {
		fmt.Println("warnings:")
		for _, w := range warnings {
			fmt.Printf("- %s\n", w)
		}
	}
	if len(critical) > 0 {
		return errors.New("config validation failed")
	}
	return nil
}

func validateApplicationReferences(
	cfg Config,
	contextName string,
	apps LifecycleConfig,
	warnings *[]string,
	critical *[]string,
) {
	actions := []struct {
		name         string
		applications []string
	}{
		{name: actionStop, applications: apps.Stop},
		{name: actionRestart, applications: apps.Restart},
		{name: actionReload, applications: apps.Reload},
		{name: actionStart, applications: apps.Start},
	}
	for _, action := range actions {
		for _, application := range action.applications {
			application = strings.TrimSpace(application)
			if application == "" {
				*warnings = append(
					*warnings,
					fmt.Sprintf("contexts.%s.apps.%s contains an empty entry", contextName, action.name),
				)
				continue
			}
			commands, ok := cfg.Applications[application]
			if !ok {
				*critical = append(
					*critical,
					fmt.Sprintf("contexts.%s.apps.%s references undefined application %q", contextName, action.name, application),
				)
				continue
			}
			hasCommand := len(applicationCommand(commands, action.name)) > 0
			hasFallback := len(commands.Stop) > 0 && len(commands.Start) > 0
			if !hasCommand && (action.name == actionRestart || action.name == actionReload) {
				hasCommand = hasFallback
			}
			if !hasCommand {
				*critical = append(
					*critical,
					fmt.Sprintf("applications.%s.%s is empty", application, action.name),
				)
			}
		}
	}
}

func isEmptyContext(ctx SwitchContext) bool {
	return ctx.MacOSNetworkLocation == "" &&
		len(ctx.DNS.NetworkServices) == 0 &&
		len(ctx.DNS.Resolvers) == 0 &&
		len(ctx.Upstreams) == 0 &&
		ctx.ProxyMode == "" &&
		ctx.ForwarderProxy == nil &&
		ctx.Alpaca == nil &&
		len(ctx.Apps.Restart) == 0 &&
		len(ctx.Apps.Stop) == 0 &&
		len(ctx.Apps.Start) == 0 &&
		len(ctx.Apps.Reload) == 0
}
