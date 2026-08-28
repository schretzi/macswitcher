## macswitcher service install

Write the LaunchAgent plist and load it

### Synopsis

Write ~/Library/LaunchAgents/com.schretzi.macswitcher.plist and load it.

Idempotent: an already-loaded job is unloaded and reloaded, so this is also
how you apply a change to the plist.

```
macswitcher service install [flags]
```

### Options

```
  -h, --help   help for install
```

### Options inherited from parent commands

```
      --binary string   path to the macswitcher executable to run (default: the running one)
      --config string   global config file (default: ~/.config/macswitcher/config.yaml)
```

### SEE ALSO

* [macswitcher service](macswitcher_service.md)	 - Manage the macswitcher LaunchAgent

