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

Nothing else. No AdGuard Home, no Privoxy, no Kerberos daemon. Unbound binds
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
  (this is not optional — see below)

**Keeping the VPN gateway zone static is not a limitation to work around —
it is the fix for a real bug.** The maintainer's own AdGuard Home-based setup
broke exactly because a VPN gateway hostname's resolution depended on which
context's upstream happened to be active: switch to a context whose upstream
is corp-internal, and the VPN gateway itself becomes unresolvable, so the
tunnel that would fix DNS can never start. A static forward-zone for that one
hostname, pointed permanently at a public resolver, doesn't have that failure
mode — it resolves the same way regardless of context, network, or whether a
VPN tunnel is currently up, or whether a switch has even run yet. Give your
VPN gateway's hostname this treatment even if you don't use a VPN context
yet; it costs three lines in `unbound.conf` and prevents a very confusing
failure later.

## Steps

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
5. `macswitcher proxy password-set` — stores the office proxy password in
   Keychain (Kerberos/NTLM negotiate is tried first automatically if your
   Mac already has a ticket; this is the fallback).
6. `macswitcher config validate`
7. `macswitcher switch home` (or `office`)

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

