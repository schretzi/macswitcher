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

	journal := newSwitchJournal(previous, selected)
	upstreamsSyncedEarly := false
	err = func() error {
		// A context whose DNS goes through the local resolver (AdGuard Home)
		// can have that resolver's upstreams rewritten to the NEW context's
		// before preflight runs, as long as nothing here depends on the
		// network the machine is still on - which is exactly the case for a
		// context that does not start a VPN. Doing this first means
		// preflight tests what the switch is actually about to leave the
		// machine on, rather than the PREVIOUS context's upstreams still
		// sitting in AdGuard Home - the bug this fixes: switching onto a
		// network whose internal names only the new context's upstreams can
		// answer failed preflight every time, because AdGuard was still
		// forwarding to the old network's resolvers and the corporate names
		// the new context needs were never going to resolve through those.
		//
		// A context that starts a VPN keeps the old order (see applyContext):
		// the tunnel has to resolve its own gateway through the resolvers
		// already in place, so rewriting AdGuard first would pull that
		// resolution onto upstreams that live behind the tunnel that has not
		// come up yet.
		if !contextStartsVPN(ctx) && len(ctx.Upstreams) > 0 {
			if err := journal.step("rewrite AdGuard Home's upstreams and restart it", func() error {
				return syncAdGuardUpstreams(cfg, ctx.Upstreams, selected)
			}); err != nil {
				return err
			}
			upstreamsSyncedEarly = true
		}
		// Preflight is a step of the switch, so it belongs in the journal:
		// "the switch never started because nothing resolved" is a different
		// story from "the switch got half-way", and the report has to be able
		// to tell them apart.
		if err := journal.step("preflight: resolve the names this context needs", func() error {
			return preflight(cfg, ctx, selected)
		}); err != nil {
			return err
		}
		return applyContext(cfgPath, cfg, ctx, selected, journal, upstreamsSyncedEarly)
	}()
	journal.close(err)

	logPath := appendSwitchLog(journal)
	if err != nil {
		reportFailedSwitch(cfgPath, cfg, journal, logPath)
		return err
	}
	logf("switched context to %s\n", selected)
	return nil
}

// reportFailedSwitch is what a failed switch does INSTEAD of rolling back.
//
// The rollback it replaces was a plausible idea that does not survive contact
// with the case it was written for. A switch fails because the machine is on a
// network the target context does not match - and the context it would roll
// back to describes a network the machine is not on either. Coming back to
// "home" while sitting in the office restores a setup that is just as dead,
// having spent the evidence of the failure on the way. Two broken states are
// not better than one, and the second one is harder to reason about because
// half of it was applied twice.
//
// So: leave the machine where it is, say exactly which steps ran and which did
// not, and write a snapshot while the broken state still exists to be
// described. The snapshot is taken automatically rather than left to a flag,
// because the moment it is needed is the moment the operator has no network to
// look up how to ask for it.
func reportFailedSwitch(cfgPath string, cfg Config, journal *switchJournal, logPath string) {
	logf("\nthe switch to %q failed at step: %s\n", journal.To, journal.FailedStep())
	logf("the machine has NOT been rolled back - the previous context describes a network\n")
	logf("this machine is not on either, so restoring it would only hide the evidence.\n")

	if logPath != "" {
		logf("switch log: %s\n", logPath)
	}
	dir, err := writeSnapshot(cfgPath, cfg, snapshotRequest{
		Reason:  fmt.Sprintf("switch %s -> %s failed", journal.From, journal.To),
		Journal: journal,
	})
	if err != nil {
		logf("warning: could not write the diagnostic snapshot: %v\n", err)
		return
	}
	logf("snapshot:   %s\n", dir)
	logf("\nWhen you have a working network again, start with %s/report.md.\n", dir)
}

// applyContext puts the machine into one context.
//
// Every step is recorded in the journal as it runs. That is not instrumentation
// bolted on afterwards: since a failed switch is no longer undone, the record of
// which steps took effect IS the recovery information. "The VPN came up, the
// resolvers were repointed, the proxy was never touched" tells the operator
// where the machine actually stands; an error string alone does not.
//
// Order matters, and not in the obvious way. Every step below depends on DNS
// still working, so nothing may break DNS before the thing that repairs it has
// run:
//
//   - The runtime state is written first because setLocalProxy, unsetLocalProxy
//     and the proxy service all read the current context back off disk to
//     decide whether the filtering proxy is in the path.
//   - Applications run before the upstreams change. A VPN has to resolve its
//     own gateway to connect, and it can only do that through the resolvers of
//     the network the machine is still on. Pointing AdGuard at the new
//     context's upstreams first is what made a forward context unswitchable:
//     those resolvers live behind the very tunnel that then could not come up,
//     leaving openconnect with "getaddrinfo failed for host puma...".
//   - The upstreams and resolvers change once the tunnel that reaches them
//     exists, and only then is DNS checked - by which point the forward proxy's
//     own name is resolvable.
//   - The proxy is changed last, so anything that fails above leaves the
//     machine on the proxy settings it already had rather than half-way onto
//     new ones.
func applyContext(cfgPath string, cfg Config, ctx SwitchContext, name string, journal *switchJournal, upstreamsSyncedEarly bool) error {
	cfg.CurrentContext = name
	if err := journal.step("persist "+name+" as the current context", func() error {
		return saveRuntimeState(cfgPath, ConfigState{CurrentContext: name})
	}); err != nil {
		return err
	}

	if location := strings.TrimSpace(ctx.MacOSNetworkLocation); location != "" {
		_ = journal.step("select the macOS network location "+location, func() error {
			if err := runCommand("scselect", location); err != nil {
				logf("warning: could not switch macOS network location: %v\n", err)
			}
			// Deliberately not returned: a failed scselect warns and the
			// switch goes on, as it always has. The journal still carries
			// the warning through the transcript.
			return nil
		})
	} else {
		journal.skip("select the macOS network location")
	}

	if err := journal.step("run the context's app and VPN hooks", func() error {
		return syncContextApplications(cfg, ctx)
	}); err != nil {
		return err
	}

	switch {
	case upstreamsSyncedEarly:
		// Already done, before preflight - see switchContext. Redoing it here
		// unconditionally would double-log a step that did not fail and,
		// worse, would mask the case where something between the two steps
		// left AdGuard Home in a different state than this rewrite expects.
		journal.skip("rewrite AdGuard Home's upstreams (already applied before preflight)")
	case len(ctx.Upstreams) > 0:
		if err := journal.step("rewrite AdGuard Home's upstreams and restart it", func() error {
			return syncAdGuardUpstreams(cfg, ctx.Upstreams, name)
		}); err != nil {
			return err
		}
	default:
		journal.skip("rewrite AdGuard Home's upstreams")
	}

	// After the upstream sync, because that restarts AdGuard Home and a
	// restart would drop a protection change made before it. Before the DNS
	// check, because on a corporate network filtering is precisely what stops
	// DNS from working - applying it afterwards would fail the switch at the
	// check and never get here.
	_ = journal.step("apply AdGuard Home's protection setting", func() error {
		syncAdGuardProtection(cfg, ctx)
		return nil
	})

	if err := journal.step("point the network services at the local resolver", func() error {
		return applyLocalResolverDNS(cfg)
	}); err != nil {
		return err
	}

	_ = journal.step("flush the system DNS cache", func() error {
		flushDNSCache()
		return nil
	})

	if err := journal.step("verify DNS resolves", func() error {
		return checkDNSResolution(ctx)
	}); err != nil {
		return fmt.Errorf("%w\nhint: DNS is not resolving after the switch; fix DNS (check AdGuard Home, VPN, network location) and rerun `macswitcher switch %s`%s", err, name, protectionHint(ctx))
	}

	return applyContextProxy(cfgPath, ctx, journal)
}

// applyContextProxy is the last phase of a switch: the proxy, changed only
// once everything it depends on is in place, so a failure above leaves the
// machine on the proxy settings it already had rather than half-way onto new
// ones.
func applyContextProxy(cfgPath string, ctx SwitchContext, journal *switchJournal) error {
	if strings.EqualFold(ctx.ProxyMode, ProxyModeOff) {
		if err := journal.step("unset the local proxy", func() error {
			return unsetLocalProxy(cfgPath)
		}); err != nil {
			return err
		}
		return journal.step("stop the proxy service", func() error {
			if err := serviceStop(); err != nil {
				logf("warning: could not stop proxy service: %v\n", err)
			}
			return nil
		})
	}
	_ = journal.step("restart the proxy service", func() error {
		if err := serviceRestart(); err != nil {
			logf("warning: could not restart proxy service automatically: %v\n", err)
		}
		return nil
	})
	return journal.step("set the local proxy", func() error {
		return setLocalProxy(cfgPath)
	})
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

// contextStartsVPN reports whether ctx's apps.start names the "vpn"
// application - the signal that DNS still has to work through the OLD
// network's resolvers before this context can do anything, because the
// tunnel has to resolve its own gateway first. Everything else is free to
// have AdGuard Home's upstreams rewritten before preflight runs.
func contextStartsVPN(ctx SwitchContext) bool {
	for _, app := range ctx.Apps.Start {
		if strings.EqualFold(strings.TrimSpace(app), "vpn") {
			return true
		}
	}
	return false
}

func status(cfgPath string) error {
	cfg, err := loadConfig(cfgPath)
	if err != nil {
		return err
	}
	proxyURL, noProxy := proxyEnvValues(cfg)
	ctx := cfg.Contexts[cfg.CurrentContext]
	logf("config: %s\n", cfgPath)
	logf("current_context: %s\n", cfg.CurrentContext)
	logf("macos_network_location: %s\n", ctx.MacOSNetworkLocation)
	logf("proxy_mode: %s\n", ctx.ProxyMode)
	logf("local_proxy: %s\n", proxyURL)
	logf("filter_proxy: %s\n", filterProxyStatusLine(cfg, ctx))
	logf("local_resolver: %s\n", cfg.DNS.LocalResolver)
	logf("adguard_upstreams_file: %s\n", cfg.AdGuard.UpstreamsFile)
	logf("adguard_filtering: %s\n", adguardProtectionStatusLine(cfg, ctx))
	logf("no_proxy: %s\n", noProxy)
	logf("network_services configured: %d (0 means auto-detect)\n", len(cfg.NetworkServices))
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
		if len(ctx.Upstreams) == 0 {
			warnings = append(warnings, fmt.Sprintf("contexts.%s.upstreams is empty", name))
		}
		// A direct context must leave the PAC to filter_proxy so its generated
		// local-network exceptions cannot drift from local_proxy.no_proxy. A
		// forward context necessarily names the corporate PAC itself.
		if filterProxyAppliesTo(cfg, ctx) && ctx.Alpaca != nil && commandNamesPAC(ctx.Alpaca.Command) {
			warnings = append(warnings, fmt.Sprintf(
				"contexts.%s.alpaca.command names its own PAC (-C) while filter_proxy applies; "+
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
	if isForwardProxyMode(ctx.ProxyMode) && ctx.ForwarderProxy != nil {
		account := ctx.ForwarderProxy.PasswordKeychainAccount
		if strings.TrimSpace(account) == "" {
			account = ctx.ForwarderProxy.Username
		}
		if _, err := keychainPasswordGet(ctx.ForwarderProxy.PasswordKeychainService, account); err != nil {
			warnings = append(warnings, "no readable keychain password found for the active forwarder proxy; run macswitcher proxy password-set")
		}
	}

	logf("config: %s\n", cfgPath)
	if len(critical) == 0 && len(warnings) == 0 {
		fmt.Println("validation: OK")
		return nil
	}
	if len(critical) > 0 {
		fmt.Println("critical:")
		for _, c := range critical {
			logf("- %s\n", c)
		}
	}
	if len(warnings) > 0 {
		fmt.Println("warnings:")
		for _, w := range warnings {
			logf("- %s\n", w)
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
