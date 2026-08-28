## macswitcher service stop

Unload the LaunchAgent

### Synopsis

Unload the job.

This is a real stop, not a kill: the plist uses KeepAlive/SuccessfulExit so
launchd does not immediately restart it. The job comes back at next login, or
on `service start`.

```
macswitcher service stop [flags]
```

### Options

```
  -h, --help   help for stop
```

### Options inherited from parent commands

```
      --binary string   path to the macswitcher executable to run (default: the running one)
      --config string   global config file (default: ~/.config/macswitcher/config.yaml)
```

### SEE ALSO

* [macswitcher service](macswitcher_service.md)	 - Manage the macswitcher LaunchAgent

