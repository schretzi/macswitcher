# Scenario A: Unbound + Alpaca (no AdGuard Home, no Privoxy)

Best fit: you want macswitcher to actually rewrite DNS upstreams per network
switch (like the AdGuard Home scenario does), but you'd rather run unbound
than install AdGuard Home.

## What this installs

| component | how | why |
| --- | --- | --- |
| `macswitcher` | `brew install schretzi/tap/macswitcher` | the switcher itself |
| `alpaca` | `brew install samuong/alpaca/alpaca` | the local forward proxy macswitcher supervises |
| `unbound` | `brew install unbound`, run via `brew services` (root, binds port 53) | the local DNS resolver, this scenario's `dns.backend` |
| a sudoers rule | `/etc/sudoers.d/macswitcher-dns-flush` | every switch flushes the DNS cache before checking it worked |
| a sudoers rule | `/etc/sudoers.d/macswitcher-unbound-restart` | every switch restarts unbound so it picks up the rewritten forwarders |

Nothing else is required. No AdGuard Home, no Privoxy; KerberosKeepAlive is
optional and covered separately below, for teams that want alpaca to
authenticate to the enterprise proxy with a Kerberos ticket instead of just
a Keychain password. Unbound binds
a dedicated loopback alias (`127.0.0.2`) instead of plain `127.0.0.1`,
because `127.0.0.1:53` is already owned by mDNSResponder on macOS —
`install.sh` creates that alias with a small LaunchDaemon so it survives a
reboot (macOS does not persist extra `lo0` aliases on its own).

## How macswitcher drives unbound here

`config.yaml` sets `dns.backend: unbound` and `unbound.forwarders_file`.
Every switch writes the active context's `upstreams` into that file (in
unbound's own `forward-zone: name: "."` / `forward-addr:` syntax) and
restarts unbound — the same mechanism the AdGuard Home scenario uses for
`adguard.upstreams_file`, just targeting unbound instead. Because
`dns.backend` is `unbound`, AdGuard Home (even if it happened to be
installed) is never opened or restarted by a switch; the two backends can
coexist, only one is ever driven.

`unbound.conf` itself is **not** rewritten by a switch — only the file it
`include`s is. Two permanent, hand-edited forward-zones stay in
`unbound.conf`:

- your corporate domain(s) → your corporate DNS servers
- your VPN gateway/forward-proxy hostname → a public resolver, explicitly

That second zone matters if you have a VPN: if its hostname's resolution
depended on whichever upstream a context happens to set, switching to a
corp-internal upstream can make the VPN gateway itself unresolvable, so the
tunnel that would fix DNS can never start. Pointing that one hostname
permanently at a public resolver sidesteps the problem entirely — it costs
three lines in `unbound.conf`. If you already run your own VPN script outside
macswitcher, add its gateway hostname here too; macswitcher doesn't need to
know about the VPN itself, only that this one name must always resolve.

## Enterprise proxy via alpaca + KerberosKeepAlive

If your Kerberos ticket is already being kept alive at a fixed ccache path
(e.g. by your own script writing to `~/.krb5cc/corp`), alpaca still needs
telling — a ticket file existing on disk is not enough by itself. macswitcher
gets that path from **KerberosKeepAlive**, a small separate daemon that owns
the ccache file and refreshes it; macswitcher just reads its config and passes
the path to alpaca as `KRB5CCNAME`. Wire it up once:

1. Install/run KerberosKeepAlive so `~/.config/kerberoskeepalive/config.yaml`
   has a profile with `ccache_path: /Users/you/.krb5cc/corp` (or wherever your
   ticket actually lands) and register it as a LaunchAgent.
2. Add it to `daemons:` in `config.yaml` so `observe` can show and (re)start it:
   ```yaml
   daemons:
     kerberos_keep_alive:
       label: com.example.kerberoskeepalive
   ```
3. Leave `forwarder_proxy.username`/`password_keychain_service` in
   `contexts/office.yaml` in place — alpaca tries Negotiate (using
   KerberosKeepAlive's ticket) first, then falls back to Basic with the
   Keychain password if the ticket is missing or expired. Without that
   fallback, an expired ticket means the proxy is simply unreachable.

Nothing else changes: the same `office` context and the same alpaca process
picks this up automatically once KerberosKeepAlive is running.

## Steps

If unbound and your own VPN script are already running outside macswitcher,
most of `install.sh` is a no-op for you — it only fills in what's missing.

1. `cd` into this directory (or copy it into your own dotfiles first — these
   files are meant to be edited and kept, not run once and thrown away).
2. Edit `unbound.conf`: replace `CHANGE-ME-corp.example.com` and the two
   `CHANGE-ME-10.0.0.x` forward addresses with your actual corporate domain
   and DNS servers, and `CHANGE-ME-vpn.corp.example.com` with your VPN
   gateway's hostname (if you have one).
3. Edit `contexts/office.yaml`: replace `CHANGE-ME-proxy.example.com`, the
   port, your username, and the two `CHANGE-ME-10.0.0.x` upstream resolvers
   with your corporate proxy's and DNS's actual values.
4. Run `./install.sh`. It installs Homebrew packages, starts Unbound as a
   system service, installs the macswitcher config/contexts, adds the
   sudoers rules, and registers macswitcher's own LaunchAgent (which
   supervises alpaca).
5. Set up Kerberos, if you want it — see "Enterprise proxy via alpaca +
   KerberosKeepAlive" above — then `macswitcher proxy password-set` to store
   the office proxy password in Keychain as the Basic fallback either way.
6. `macswitcher config validate`
7. `macswitcher switch home` (or `office`)
8. `macswitcher observe` — unbound (and, once configured, kerberoskeepalive)
   both show up here for at-a-glance status and restart.

Re-running `install.sh` after any edit is the supported way to apply a
change — it's idempotent and backs up anything it would overwrite.

## Verifying it worked

```sh
macswitcher status
macswitcher observe                                   # unbound row shows current forward-addr entries
dig +short @127.0.0.2 your-corp-domain.example.com   # should answer once on VPN/corp network
dig +short @127.0.0.2 your-vpn-gateway.example.com    # should answer from ANY network
cat /opt/homebrew/etc/unbound/conf.d/forwarders.conf  # rewritten by the last switch
brew services info unbound
```

## Testing unbound without losing an existing AdGuard Home setup

If you already run AdGuard Home and just want to try unbound side by side:
install unbound (`brew install unbound`, this scenario's loopback alias and
`unbound.conf`) without touching your existing `adguard:` config, then set
`dns.backend: unbound` and `dns.local_resolver: 127.0.0.2` in your own
`config.yaml` and switch. AdGuard Home keeps running, untouched, on its own
alias (`127.0.0.3`) and its `upstreams_file` is never opened while
`dns.backend` is `unbound`. Set `dns.backend` back to `adguard` (and
`dns.local_resolver` back to `127.0.0.3`) to switch back — nothing to
reinstall on either side.

