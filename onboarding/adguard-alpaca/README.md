# Scenario B: AdGuard Home + Alpaca (no Privoxy)

Best fit: your corporate/VPN DNS servers actually change (different sites,
a VPN concentrator with its own resolvers), and you want macswitcher to keep
AdGuard Home's upstream DNS servers in sync with whichever context is active,
automatically, on every switch.

## What this installs

| component | how | why |
| --- | --- | --- |
| `macswitcher` | `brew install schretzi/tap/macswitcher` | the switcher itself |
| `alpaca` | `brew install samuong/alpaca/alpaca` | the local forward proxy macswitcher supervises |
| AdGuard Home | downloaded directly from the [upstream GitHub releases](https://github.com/AdguardTeam/AdGuardHome), run as a LaunchDaemon | the local DNS resolver whose upstreams macswitcher rewrites per context |
| a loopback alias (`127.0.0.2`) | small LaunchDaemon, `install.sh` | `127.0.0.1:53` is already owned by mDNSResponder on macOS |
| sudoers rules | `/etc/sudoers.d/macswitcher-dns-flush`, `/etc/sudoers.d/macswitcher-onboarding-adguardhome-restart` | every switch flushes the DNS cache and restarts AdGuard Home after rewriting its upstreams |

No Privoxy (no TLS-inspecting filter — `proxy_mode: direct` still works,
alpaca just reaches the internet directly), no Kerberos daemon (the office
context uses a Keychain-backed username/password instead; add
KerberosKeepAlive later with no change here if you want Negotiate auth).

## Why the VPN gateway hostname needs a per-domain override

`upstreams.conf` has two parts: a default block macswitcher rewrites from
each context's `upstreams:` list, and per-domain `[/zone/]address` lines it
never touches. Put your VPN gateway/forward-proxy hostname in the second
part, pointed at a public resolver, from day one — even before you have a
VPN context.

Here's the failure this avoids: if that hostname instead relied on the
default upstream, it would resolve fine right up until you switch to a
context whose upstreams are corp-internal-only. If that context is also the
one that's supposed to bring up a VPN tunnel using that same hostname, the
tunnel can never start — its own gateway address depends on DNS servers only
reachable *through* the tunnel it's trying to bring up. This exact bug is
what prompted this override to exist in the first place. A per-domain line
pointed at a public resolver breaks that circular dependency permanently,
regardless of which context is active or whether a tunnel is currently up.

## Steps

1. `cd` into this directory (or copy it into your own dotfiles first).
2. Edit `upstreams.conf`: replace `CHANGE-ME-vpn.corp.example.com` with your
   VPN gateway/forward-proxy hostname (delete the two `[/…/]` lines if you
   don't have one).
3. Edit `contexts/office.yaml`: replace `CHANGE-ME-proxy.example.com`, the
   port, your username, and the two `CHANGE-ME-10.0.0.x` corporate DNS
   servers with your employer's actual values.
4. Run `./install.sh`. It installs Homebrew packages, creates the loopback
   alias, downloads and bootstraps AdGuard Home (generating an admin account
   whose password lands in Keychain, never in a file), installs the
   macswitcher config/contexts, adds both sudoers rules, and registers
   macswitcher's own LaunchAgent (which supervises alpaca).
   - If the automated AdGuard Home bootstrap fails (an API shape mismatch
     against whatever version was just downloaded is the most likely
     cause), the script tells you to finish it by hand at
     `http://127.0.0.2:3053` — the one-time setup wizard is exactly the same
     either way.
5. `macswitcher proxy password-set` — stores the office proxy password in
   Keychain.
6. `macswitcher config validate`
7. `macswitcher switch home` (or `office`)

Re-running `install.sh` after editing `config.yaml`/`contexts/*.yaml` is
supported and safe. It will **not** re-seed `upstreams.conf` or re-run the
AdGuard Home first-run setup once they exist — those are one-time steps,
same as AdGuard Home's own web UI wouldn't want its config silently reset
under it.

## Verifying it worked

```sh
macswitcher status
dig +short @127.0.0.2 google.com                       # via the active context's upstreams
dig +short @127.0.0.2 your-vpn-gateway.example.com      # should answer from ANY network/context
sudo launchctl print system/com.macswitcher.onboarding.adguardhome | head -5
```

## Testing unbound without losing this AdGuard Home setup

`dns.backend: adguard` (set explicitly in `config.yaml`, even though it's
also the default) is what makes AdGuard Home the one a switch rewrites and
restarts. Installing
[`onboarding/unbound-alpaca/`](../unbound-alpaca/)'s unbound alongside this
scenario, on its own loopback alias (`127.0.0.2` for unbound vs. this
scenario's own alias for AdGuard Home — pick two distinct ones if you install
both), does not affect this AdGuard Home setup at all: `unbound.forwarders_file`
is never opened while `dns.backend` stays `adguard`. To actually try unbound,
set `dns.backend: unbound` and point `dns.local_resolver` at unbound's alias,
then switch. Set both back to flip back to AdGuard Home — nothing to
reinstall on either side.
