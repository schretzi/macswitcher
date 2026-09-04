## macswitcher preflight

Check that a context's DNS prerequisites resolve, without switching

### Synopsis

Resolve every name a switch into CONTEXT depends on - dns.check_host, the
forward proxy's hostname, and anything in preflight_hosts - and report what
fails. If nothing resolves, the resolvers are handed back to DHCP for one
more attempt and then restored, so a failure says whether the network or the
DNS configuration is at fault. Nothing else is changed.

```
macswitcher preflight CONTEXT [flags]
```

### Options

```
  -h, --help   help for preflight
```

### Options inherited from parent commands

```
      --config string   global config file (default: ~/.config/macswitcher/config.yaml)
```

### SEE ALSO

* [macswitcher](macswitcher.md)	 - Manage macOS network contexts, DNS, and proxy services

