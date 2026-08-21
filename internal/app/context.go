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
	cfg.CurrentContext = selected
	if err := saveRuntimeState(cfgPath, ConfigState{CurrentContext: selected}); err != nil {
		return err
	}

	if strings.TrimSpace(ctx.MacOSNetworkLocation) != "" {
		if err := runCommand("scselect", ctx.MacOSNetworkLocation); err != nil {
			fmt.Printf("warning: could not switch macOS network location: %v\n", err)
		}
	}
	if len(ctx.UnboundForwarders) > 0 {
		if err := writeUnboundForwarders(cfg, ctx.UnboundForwarders); err != nil {
			return err
		}
	}
	if err := applyLocalResolverDNS(cfg); err != nil {
		return err
	}
	if strings.EqualFold(ctx.ProxyMode, "off") {
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
	if err := syncContextApplications(cfg, ctx); err != nil {
		return err
	}
	fmt.Printf("switched context to %s\n", selected)
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
	fmt.Printf("local_resolver: %s\n", cfg.DNS.LocalResolver)
	fmt.Printf("unbound_forwarders_file: %s\n", cfg.Unbound.ForwardersFile)
	fmt.Printf("no_proxy: %s\n", noProxy)
	fmt.Printf("network_services configured: %d (0 means auto-detect)\n", len(cfg.NetworkServices))
	fmt.Println("service:")
	return serviceStatus()
}

func configValidate(cfgPath string) error {
	cfg, err := loadConfig(cfgPath)
	if err != nil {
		return err
	}
	critical := make([]string, 0)
	warnings := make([]string, 0)
	for name, ctx := range cfg.Contexts {
		if isEmptyContext(ctx) {
			continue
		}
		if strings.TrimSpace(ctx.ProxyMode) == "" {
			warnings = append(warnings, fmt.Sprintf("contexts.%s.proxy_mode is empty (recommended: direct|off)", name))
		}
		if ctx.ForwarderProxy != nil {
			proxy := *ctx.ForwarderProxy
			if err := validateForwarderProxy(proxy); err != nil {
				critical = append(critical, fmt.Sprintf("contexts.%s.forwarder_proxy: %v", name, err))
			}
			if strings.TrimSpace(proxy.PasswordKeychainAccount) == "" {
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
		if len(ctx.UnboundForwarders) == 0 {
			warnings = append(warnings, fmt.Sprintf("contexts.%s.unbound_forwarders is empty", name))
		}
		validateApplicationReferences(cfg, name, ctx.Apps, &warnings, &critical)
	}

	ctx := cfg.Contexts[cfg.CurrentContext]
	if ctx.ForwarderProxy != nil {
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
	apps AppLifecycleConfig,
	warnings *[]string,
	critical *[]string,
) {
	actions := []struct {
		name         string
		applications []string
	}{
		{name: "stop", applications: apps.Stop},
		{name: "restart", applications: apps.Restart},
		{name: "reload", applications: apps.Reload},
		{name: "start", applications: apps.Start},
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
			if !hasCommand && (action.name == "restart" || action.name == "reload") {
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
		len(ctx.UnboundForwarders) == 0 &&
		ctx.ProxyMode == "" &&
		ctx.ForwarderProxy == nil &&
		ctx.Alpaca == nil &&
		ctx.Kerberos == (KerberosConfig{}) &&
		len(ctx.Apps.Restart) == 0 &&
		len(ctx.Apps.Stop) == 0 &&
		len(ctx.Apps.Start) == 0 &&
		len(ctx.Apps.Reload) == 0
}
