package app

import (
	"github.com/spf13/cobra"
)

var configFile string

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
	)
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
		RunE: func(cmd *cobra.Command, args []string) error {
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
		RunE: func(cmd *cobra.Command, args []string) error {
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
			RunE: func(cmd *cobra.Command, args []string) error {
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
			RunE: func(cmd *cobra.Command, args []string) error {
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
			RunE: func(cmd *cobra.Command, args []string) error {
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
			RunE: func(cmd *cobra.Command, args []string) error {
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
		RunE: func(cmd *cobra.Command, args []string) error {
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
			RunE: func(cmd *cobra.Command, args []string) error {
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
			RunE: func(cmd *cobra.Command, args []string) error {
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

func serviceCommand() *cobra.Command {
	command := &cobra.Command{
		Use:     "service",
		Short:   "Manage the launchd Alpaca proxy service",
		GroupID: "service",
	}
	for name, spec := range map[string]struct {
		short string
		run   func() error
	}{
		"install":   {short: "Install the launchd user service", run: serviceInstall},
		"uninstall": {short: "Uninstall the launchd user service", run: serviceUninstall},
		"start":     {short: "Start the launchd user service", run: serviceStart},
		"stop":      {short: "Stop the launchd user service", run: serviceStop},
		"restart":   {short: "Restart the launchd user service", run: serviceRestart},
		"status":    {short: "Show launchd user service status", run: serviceStatus},
	} {
		entry := spec
		command.AddCommand(&cobra.Command{
			Use:   name,
			Short: spec.short,
			RunE: func(cmd *cobra.Command, args []string) error {
				return entry.run()
			},
		})
	}
	return command
}
