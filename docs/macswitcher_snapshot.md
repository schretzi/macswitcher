## macswitcher snapshot

Write a diagnostic snapshot of the current network state

### Synopsis

Collect the context transition, every daemon's state and log tail, the
resolvers and routes actually in effect, DNS probe results and the
relevant configuration into ~/Library/Logs/macswitcher-snapshots/<time>/.

Read-only: it changes nothing, and every command it runs is time-bounded,
so it stays usable on a machine whose network is already broken.

```
macswitcher snapshot [flags]
```

### Options

```
  -h, --help            help for snapshot
      --reason string   one line on why this snapshot was taken, recorded in the report
```

### Options inherited from parent commands

```
      --config string   global config file (default: ~/.config/macswitcher/config.yaml)
```

### SEE ALSO

* [macswitcher](macswitcher.md)	 - Manage macOS network contexts, DNS, and proxy services

