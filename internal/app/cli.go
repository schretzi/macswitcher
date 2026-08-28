package app

import (
	"github.com/schretzi/macswitcher/internal/version"

	"github.com/spf13/cobra"
)

var configFile string

// appName is the binary name: it drives the launchd label, the log file
// names and the `version` output.
const appName = "macswitcher"

// licenseNotice is printed by `macswitcher version`.
const licenseNotice = `Copyright (C) 2026 Martin Fuchsluger
License: MIT <https://opensource.org/licenses/MIT>.
This is free software: you are free to change and redistribute it.
There is NO WARRANTY, to the extent permitted by law.`

// Execute runs the macswitcher command-line application.
func Execute() error {
	return newRootCommand().Execute()
}

// Root returns the root cobra command, used by the docs generator to walk
// the command tree without executing it.
func Root() *cobra.Command {
	return newRootCommand()
}

func newRootCommand() *cobra.Command {
	root := &cobra.Command{
		Use:               "macswitcher",
		Short:             "Manage macOS network contexts, DNS, and proxy services",
		SilenceUsage:      true,
		SilenceErrors:     true,
		DisableAutoGenTag: true,
	}
	root.PersistentFlags().StringVar(
		&configFile,
		"config",
		"",
		"global config file (default: ~/.config/macswitcher/config.yaml)",
	)
	root.AddGroup(
		&cobra.Group{ID: "context", Title: "Context commands:"},
		&cobra.Group{ID: "proxy", Title: "Proxy commands:"},
		&cobra.Group{ID: "config", Title: "Configuration commands:"},
		&cobra.Group{ID: "service", Title: "Service commands:"},
	)

	root.AddCommand(
		contextCommand(),
		statusCommand(),
		proxyCommand(),
		configCommand(),
		serviceCommand(),
		observeCommand(),
		version.NewCommand(appName, licenseNotice),
	)

	// `--version` and `version` report the same thing, from the same place.
	root.Version = version.String(appName)
	return root
}

func configuredPath() (string, error) {
	if configFile != "" {
		return configFile, nil
	}
	return configPath()
}

func contextCommand() *cobra.Command {
	command := &cobra.Command{
		Use:     "switch CONTEXT",
		Aliases: []string{"context"},
		Short:   "Switch the active network context",
		Args:    cobra.ExactArgs(1),
		GroupID: "context",
		RunE: func(_ *cobra.Command, args []string) error {
			path, err := configuredPath()
			if err != nil {
				return err
			}
			return switchContext(path, args)
		},
	}
	return command
}

func statusCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show the active context and service status",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			path, err := configuredPath()
			if err != nil {
				return err
			}
			return status(path)
		},
	}
}

func proxyCommand() *cobra.Command {
	command := &cobra.Command{
		Use:     "proxy",
		Short:   "Manage local proxy wiring and proxy diagnostics",
		GroupID: "proxy",
	}
	command.AddCommand(
		&cobra.Command{
			Use:   "set",
			Short: "Set the local proxy in macOS, zsh, and Docker",
			RunE: func(_ *cobra.Command, _ []string) error {
				path, err := configuredPath()
				if err != nil {
					return err
				}
				return setLocalProxy(path)
			},
		},
		&cobra.Command{
			Use:   "unset",
			Short: "Unset the local proxy in macOS, zsh, and Docker",
			RunE: func(_ *cobra.Command, _ []string) error {
				path, err := configuredPath()
				if err != nil {
					return err
				}
				return unsetLocalProxy(path)
			},
		},
		detectAuthCommand(),
		&cobra.Command{
			Use:   "run",
			Short: "Run the configured Alpaca proxy runtime",
			RunE: func(_ *cobra.Command, _ []string) error {
				path, err := configuredPath()
				if err != nil {
					return err
				}
				return runProxy(path)
			},
		},
		&cobra.Command{
			Use:   "password-set",
			Short: "Save the active forwarder proxy password in macOS Keychain",
			RunE: func(_ *cobra.Command, _ []string) error {
				path, err := configuredPath()
				if err != nil {
					return err
				}
				return keychainPasswordSet(path)
			},
		},
	)
	return command
}

func detectAuthCommand() *cobra.Command {
	var proxy, target string
	command := &cobra.Command{
		Use:   "detect-auth",
		Short: "Probe upstream proxy authentication methods",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			path, err := configuredPath()
			if err != nil {
				return err
			}
			probeArgs := []string{"--url", target}
			if proxy != "" {
				probeArgs = append(probeArgs, "--proxy", proxy)
			}
			return detectAuth(path, probeArgs)
		},
	}
	command.Flags().StringVar(&proxy, "proxy", "", "upstream proxy host:port")
	command.Flags().StringVar(&target, "url", "https://example.com", "target URL for the probe")
	return command
}

func configCommand() *cobra.Command {
	command := &cobra.Command{
		Use:     "config",
		Short:   "Create and validate configuration",
		GroupID: "config",
	}
	command.AddCommand(
		&cobra.Command{
			Use:   "init",
			Short: "Create global and starter context configuration files",
			RunE: func(_ *cobra.Command, _ []string) error {
				path, err := configuredPath()
				if err != nil {
					return err
				}
				return initConfig(path)
			},
		},
		&cobra.Command{
			Use:   "validate",
			Short: "Validate global and context configuration",
			RunE: func(_ *cobra.Command, _ []string) error {
				path, err := configuredPath()
				if err != nil {
					return err
				}
				return configValidate(path)
			},
		},
	)
	return command
}

func observeCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "observe",
		Short: "Open a TUI showing Alpaca, Unbound, KerberosKeepAlive, and omt status",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			path, err := configuredPath()
			if err != nil {
				return err
			}
			return Observe(path)
		},
	}
}

// serviceCommand lives in service.go, built from the shared internal/service
// package so the subtree is identical across macswitcher, kerberoskeepalive
// and omt. Ranging over a map to build subcommands also produced a
// non-deterministic order in --help and generated docs.
