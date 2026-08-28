## macswitcher service

Manage the macswitcher LaunchAgent

### Synopsis

Manage the launchd job that runs macswitcher in the background.

  label   com.schretzi.macswitcher
  plist   ~/Library/LaunchAgents/com.schretzi.macswitcher.plist
  log     ~/Library/Logs/macswitcher.log
  stderr  ~/Library/Logs/macswitcher.err.log

Both logs are rotated by newsyslog, configured in MacbookSetup under
etc/newsyslog.d/macswitcher.conf.

### Options

```
      --binary string   path to the macswitcher executable to run (default: the running one)
  -h, --help            help for service
```

### Options inherited from parent commands

```
      --config string   global config file (default: ~/.config/macswitcher/config.yaml)
```

### SEE ALSO

* [macswitcher](macswitcher.md)	 - Manage macOS network contexts, DNS, and proxy services
* [macswitcher service install](macswitcher_service_install.md)	 - Write the LaunchAgent plist and load it
* [macswitcher service restart](macswitcher_service_restart.md)	 - Unload and reload the LaunchAgent
* [macswitcher service start](macswitcher_service_start.md)	 - Load the LaunchAgent
* [macswitcher service status](macswitcher_service_status.md)	 - Show whether the LaunchAgent is installed, loaded and running
* [macswitcher service stop](macswitcher_service_stop.md)	 - Unload the LaunchAgent
* [macswitcher service uninstall](macswitcher_service_uninstall.md)	 - Unload the LaunchAgent and remove its plist

