# Onboarding: a minimal macswitcher setup

The maintainer's own machine runs macswitcher wired into a large, personal
Ansible repository (AdGuard Home, Unbound history, Privoxy with TLS
inspection, KerberosKeepAlive, VPN tunnels, SSH port-forwarding, per-domain
DNS overlays…). None of that is required to use macswitcher for the one thing
most teammates actually want: **switch between "home" and "office" and have
DNS and the corporate proxy follow along automatically.**

This directory is a from-scratch, Ansible-free path to that. It ships plain
shell scripts and plain config files, not a role in someone else's playbook.

## What macswitcher actually needs

Reading `internal/app` rather than any one person's setup, macswitcher's own
requirements are small:

| requirement | why | unavoidable? |
| --- | --- | --- |
| a local DNS resolver at a fixed loopback address | `dns.local_resolver` is what network services get pointed at on every switch | yes |
| `alpaca` (https://github.com/samuong/alpaca) on `PATH` | the local forward proxy macswitcher supervises via its own LaunchAgent | yes, if you want proxy switching at all |
| passwordless `sudo` for `dscacheutil -flushcache` and `killall -HUP mDNSResponder` | every switch flushes the DNS cache before checking it worked | yes |
| a corporate/forward proxy's host, port and auth | only if `proxy_mode: forward` is ever used | no — `direct`/`off` contexts don't need one |

Everything else — AdGuard Home, Privoxy, Kerberos, VPN, per-domain DNS
overlays, system-wide loopback aliases — is optional machinery this
maintainer's own network happens to need, not something macswitcher requires.
In particular:

- **Both AdGuard Home and unbound can be macswitcher's `dns.backend`.**
  `dns.backend: adguard` (the default) or `dns.backend: unbound` picks which
  one a switch rewrites and restarts; the other, even if installed and
  running, is left completely untouched. Both `adguard.upstreams_file` and
  `unbound.forwarders_file` can be configured at the same time, which is what
  lets you set one up as your daily driver and try the other without losing
  it — see "Testing the other backend" in each scenario's README.
- **Privoxy** exists only to add TLS-inspecting URL filtering in `direct`
  mode. Skip it entirely and `proxy_mode: direct` still works — alpaca just
  reaches the internet itself instead of through a filter.
- **A dedicated loopback alias** is still needed even for a single local
  resolver — `127.0.0.1:53` is already owned by mDNSResponder on macOS — but
  it only needs to be *one* alias (`127.0.0.2` for unbound, `127.0.0.3` for
  AdGuard Home), not the block of a dozen this maintainer's machine uses for
  other, unrelated local services. Both scenarios below install a small
  LaunchDaemon that creates just that one alias at boot.

## Two supported scenarios

| | [`unbound-alpaca/`](unbound-alpaca/) | [`adguard-alpaca/`](adguard-alpaca/) |
| --- | --- | --- |
| local resolver | unbound (`dns.backend: unbound`, Homebrew formula) | AdGuard Home (`dns.backend: adguard`, GitHub release) |
| filtering proxy | none | none |
| forward proxy | alpaca | alpaca |
| per-context DNS upstream rewriting | yes — unbound's `forwarders_file` is rewritten on every switch | yes — AdGuard's `upstream_dns_file` is rewritten on every switch |
| best fit | "I want per-context DNS rewriting, but I'd rather run unbound than AdGuard Home" | "I want per-context DNS rewriting and don't mind AdGuard Home's extra moving parts (filtering, web UI, API)" |

Both give you: `macswitcher switch home`, `macswitcher switch office`, working
DNS that follows the active context's `upstreams`, and the corporate forward
proxy (with Kerberos/NTLM/Basic auth) applied automatically — the two
problems this tool exists to solve. Since only one backend is ever driven by
`dns.backend` at a time, it's entirely possible (if more to maintain) to
install both scenarios' resolvers side by side and flip between them; each
scenario's README covers that under "Testing the other backend".

## How to use these

1. Read [`unbound-alpaca/README.md`](unbound-alpaca/README.md) or
   [`adguard-alpaca/README.md`](adguard-alpaca/README.md) and pick one.
2. Copy that scenario's directory somewhere durable (a personal dotfiles repo
   is a good place — these files are meant to be edited, not run once and
   discarded).
3. Fill in the placeholders (`corp-proxy.example.com`, `corp.example.com`,
   your username, etc.) with your actual employer's values.
4. Run `./install.sh`. It is idempotent — re-running it after editing a
   config file is the expected way to apply a change.
5. `macswitcher config validate`, then `macswitcher switch home`.

Nothing here touches the maintainer's own Ansible repository, and nothing in
that repository is required to use it.
