# macswitcher

`macswitcher` switches macOS network contexts and keeps DNS, Unbound, proxy,
Alpaca, Kerberos, and application lifecycle settings together for each location.

## Build

```sh
go build -o macswitcher ./cmd/macswitcher
```

Requires macOS, Go 1.23+, `alpaca`, and optionally Docker Desktop and Unbound.

## Install

```sh
brew install schretzi/tap/macswitcher
```

(published to [schretzi/homebrew-tap](https://github.com/schretzi/homebrew-tap)
by the release pipeline; see [Releasing](#releasing) below)

## Configuration

Installation is managed by the MacbookSetup Ansible repository. The CLI reads
the linked global file and context files from:

```text
~/.config/macswitcher/config.yaml
~/.config/macswitcher/contexts/home.yaml
~/.config/macswitcher/contexts/remote.yaml
~/.config/macswitcher/contexts/work.yaml
```

`config.yaml` contains app-global settings such as the local proxy,
network-service defaults, application lifecycle commands (including Docker),
and the shared Alpaca binary and command. Context files contain only
location-specific overrides and references to those named applications. The
private and work overlays supply their own context files; the public repository
contains only generic defaults and an empty work starter.

```yaml
mac_network_location: Automatic
dns:
  network_services: []
  resolvers:
    - 127.0.0.2
  # Optional. The name a switch resolves to decide whether DNS came up.
  check_host: intranet-host.example.com
  # Optional. DNS search list, completing single-label names.
  search_domains:
    - corp.example.com
proxy_mode: direct
# Optional. Turns AdGuard Home's filtering on or off for this context.
# Omit it and macswitcher leaves filtering exactly as it found it.
protection_enabled: false
apps:
  reload:
    - unbound
```

`dns.check_host` overrides the name a switch resolves to prove DNS is working
before it goes any further. The defaults are the forward proxy's own hostname
in `forward` mode and `google.com` otherwise, and both assume the network
resolves public names — which a corporate network reached over a VPN need not
do. Where its resolvers serve the intranet only, `google.com` tests something
the network was never going to answer, and a switch that in fact worked is
reported as failed. Point `check_host` at an intranet name that is always
resolvable on the network the context describes. An explicit value wins over
the forward proxy's hostname.

`dns.search_domains` sets the DNS search list, which completes single-label
names. A corporate PAC may nominate its proxy by short name — `PROXY
proxy:8080` — and nothing but the search list can turn that into a resolvable
address. Without it every request through that proxy fails, and because the
failure is reported by the local proxy rather than by DNS it arrives as a
`502 Bad Gateway`, naming the wrong culprit entirely.

The list is applied on every switch, including when a context names none: in
that case it is *cleared*. This is deliberate. A search domain that outlived
the context needing it goes on completing short names against a network the
machine has since left, which resolves them to nothing or, worse, to something
unintended on the current network.

The global and context configuration files use YAML. Runtime context selection
is kept separately in `state.json`; Docker's own `~/.docker/config.json` also
remains JSON. The global file contains shared local-proxy, Alpaca, DNS, Unbound,
and application commands. Context files contain the active mode,
optional `forwarder_proxy` settings, and application action lists. Actions are
explicit: `start`, `stop`, `restart`, and `reload`. A reload is used when the
application supports hot reloading; the switcher does not automatically stop
and start applications for every context change. If an application has no
explicit `restart` or `reload` command, but has both `stop` and `start`
commands, those are used as a fallback.

### The launchd environment

Setting the proxy also publishes it into the user's launchd GUI domain with
`launchctl setenv`, alongside the system, shell and Docker settings.

This exists because launchd does not source the shell's rc files. A LaunchAgent
starts with an environment holding little more than `PATH`, so the
`~/.zsh/rcs/proxy` file every interactive shell reads is invisible to it — an
agent that works perfectly when you run it by hand fails as a background job,
which is a confusing way to find out.

It is worth knowing what that failure looks like. A Go program whose transport
has no proxy resolves the target host itself; with a proxy it never resolves
the name at all and hands it to the proxy instead. So a missing proxy surfaces
as `lookup oauth2.googleapis.com: no such host` — a DNS error for a problem
that has nothing to do with DNS.

`launchctl setenv` only reaches processes started *after* it runs, and Go
caches the environment on its first `http.ProxyFromEnvironment` call, so a
running agent cannot pick up a change either way. Agents that consume the proxy
therefore have to be restarted, and are named in `local_proxy.restart_agents`:

```yaml
local_proxy:
  host: 127.0.0.1
  port: 3128
  restart_agents:
    - tunneling
```

Each entry names an `applications` key and is restarted after the proxy is
applied. That is deliberately not a context's `apps.restart` list: those run
early in a switch, before DNS and long before the proxy, so an agent restarted
there would inherit the environment of the context being left. Nor does the
list vary by context — an agent that consumes the proxy needs the restart in
every context, including the one that turns the proxy off.

### Bypassing the proxy

There are four independent places a host can be excluded from the proxy, and
they do not use the same syntax or apply in the same modes. Getting one of them
right is usually not enough.

| layer | reaches | applies in |
| --- | --- | --- |
| `NO_PROXY` (shell, Docker, launchd) | CLI tools and anything using Go/curl conventions | every mode |
| macOS bypass list | GUI applications and browsers | every mode |
| `filter_proxy.direct` | traffic through the filtering proxy | **`direct` mode only** |
| the upstream PAC | traffic alpaca forwards | `forward` mode |

Both `NO_PROXY` and the macOS bypass list are generated from a single
`local_proxy.no_proxy` list, because keeping two hand-maintained lists in step
is a losing game.

The one that catches people out is `filter_proxy.direct`. The generated filter
PAC is only in the path in `direct` mode (`filterProxyAppliesTo`) — in
`forward` mode alpaca reads the *corporate* PAC instead, so an entry added
there has no effect under VPN. It is the right place for hosts that must skip
TLS inspection, and the wrong place for hosts that must skip the proxy.

That is why the macOS bypass list is managed here rather than left to the
upstream PAC: it works in every mode, needs no cooperation from a PAC file
nobody controls, and preserves alpaca's automatic PAC refresh.

Note the two syntaxes differ in a way that fails quietly. Go matches
`no_proxy` entries against domain *labels*, so `kiac` covers `kiac` and
`*.kiac` — but not `gtt.apps.main.kiac.example.net`, which needs its own
`.kiac.example.net` entry. `networksetup` matches literally unless there is a
`*`, so each name is written out in both forms.

`proxy_mode` accepts exactly three values:

- `off` — removes all proxy configuration (system network services, the macOS
  bypass list, the `~/.zsh/rcs/proxy` shell env file, Docker's
  `~/.docker/config.json`, and the launchd environment) and stops the local
  proxy service.
- `direct` (default) — points system settings at the local proxy, and the
  local proxy reaches the internet directly. Any `forwarder_proxy` block is
  ignored in this mode.
- `forward` — points system settings at the local proxy, and the local proxy
  forwards requests to the upstream proxy defined in the required
  `forwarder_proxy` block. `macswitcher config validate` fails if
  `proxy_mode: forward` is set without a `forwarder_proxy` block.

Forwarder credentials are either read from the macOS Keychain (NTLM/Basic) or
taken from a Kerberos ticket file — see below.

A `forwarder_proxy` block belongs to a context file, for example
`~/.config/macswitcher/contexts/work.yaml`. `proxy_server` and `port` are
always required and identify the upstream proxy macswitcher forwards to; the
rest of the block depends on how that proxy authenticates.

**NTLM/Basic (Keychain-backed password)**:

```yaml
proxy_mode: forward
forwarder_proxy:
  proxy_server: proxy.example.com
  port: 8080
  username: jdoe
  password_keychain_service: macswitcher-work-proxy
  password_keychain_account: jdoe
  pac_file: https://proxy.example.com/proxy.pac
  auth_allowlist:
    - proxy.example.com
```

- `username` and `password_keychain_service` are required; the password itself
  is never stored in the config and is read from Keychain at runtime (see
  `proxy password-set` below). `password_keychain_account` is optional and
  defaults to `username` when omitted.
- `pac_file` is optional and, if set, points to the PAC URL to configure for
  this context.
- `auth_allowlist` is optional but strongly recommended: it restricts which
  proxy hosts may receive the stored credentials, even if a PAC file selects a
  different host. Leaving it empty, or using `*`, is permissive and triggers a
  `config validate` warning.

**Kerberos/Negotiate**: configure the ticket location once in
KerberosKeepAlive.

alpaca authenticates to the forward proxy by trying Negotiate, then NTLM, then
Basic, and drops any method it has no credentials for. macOS GSS uses its
default credential cache, while KerberosKeepAlive deliberately refreshes the
`ccache_path` named in its own profile. In forward contexts macswitcher passes
that path as `KRB5CCNAME=FILE:<ccache_path>` to alpaca, so its Negotiate token
uses the ticket KKA actually maintains. Keep the ticket alive with
KerberosKeepAlive and it is used automatically.

macswitcher's part is the last rung: it reads the `forwarder_proxy` password
from the Keychain and passes it to alpaca as `BASIC_CREDENTIALS`. Without it a
context whose ticket has expired — or whose KDC cannot be discovered — has no
usable authentication method at all, and every request through the proxy
fails. Basic is slower, authenticating per request, and weaker, which is why
it is last and not first.

The password is passed through the environment rather than on the command
line: argv is world-readable via `ps`, an environment is not (`ps -E` shows
another user's environment only to root).

There is no `ticket_file` setting. It used to exist and conflated two separate
questions — where the ticket lives, and whether the proxy authenticates with
it. `observe` and alpaca's `KRB5CCNAME` both read the location from
KerberosKeepAlive's own `ccache_path`, which is the thing that creates the
file.

cntlm is likewise gone, along with the `{{cntlm_conf}}` template token. alpaca
speaks NTLM directly; there is nothing left for a bridge to do.

The public config contains no environment-specific IPs, hostnames, usernames,
PAC URLs, or proxy credentials. Put those values in the private/work overlays.
Secrets (Keychain passwords and Kerberos ticket files) are never configuration
file values.

For a machine not managed by Ansible, `macswitcher config init` creates a
`home` context from the current macOS network location, network services, DNS
resolver, and the default upstreams already in AdGuard Home's
`upstreams_file` (its default resolver, `dns.backend: adguard`), plus an
intentionally empty `work` context.

### Choosing a DNS backend: AdGuard Home or unbound

`dns.backend` picks which local resolver a context switch actually rewrites
and restarts: `adguard` (the default) or `unbound`. Only the selected
backend is ever touched — the other one, even if fully configured and
running, is left completely alone. This is what makes it safe to set both
up at once and flip between them:

```yaml
dns:
  local_resolver: 127.0.0.3   # match whichever backend is selected below
  backend: adguard            # or: unbound
adguard:
  upstreams_file: /etc/adguardhome/upstreams.conf
unbound:
  forwarders_file: /opt/homebrew/etc/unbound/conf.d/forwarders.conf
```

Both `adguard.upstreams_file` and `unbound.forwarders_file` may be set at the
same time — that is the point. A machine can keep AdGuard Home as its daily
driver (`dns.backend: adguard`) while unbound sits installed and running
alongside it on its own loopback alias, never written to. To actually try
unbound: set `dns.backend: unbound`, point `dns.local_resolver` at unbound's
own alias (its Ansible-installed default is `127.0.0.2`, distinct from
AdGuard Home's `127.0.0.3`), and switch — AdGuard Home's `upstreams_file` is
never opened, and its own daemon is never restarted. Set `dns.backend` back
to `adguard` to switch back, with nothing to reinstall or reconfigure on
either side.

`dns.backend` must be `adguard` or `unbound`; anything else, including a
typo, fails `macswitcher config validate` and every switch loudly rather than
silently keeping the previous switch's resolvers in place.

Unbound's forwarders are managed at `unbound.forwarders_file`, e.g.
`/opt/homebrew/etc/unbound/conf.d/forwarders.conf`. Ansible initially links
this path to its default `Dotfiles/unbound/forwarders.conf`. Before writing
context-specific forwarders, macswitcher removes only that symlink and creates
a real runtime-managed file, so the repository source is never modified —
this applies only while `dns.backend` is `unbound`; while it is `adguard`,
the forwarders file is never opened at all.

Store forwarder proxy credentials in Keychain:

```sh
./macswitcher proxy password-set
```

Validate all global and context files with `./macswitcher config validate`.

## Commands

```sh
./macswitcher switch home
./macswitcher switch remote
./macswitcher switch work
./macswitcher preflight work
./macswitcher snapshot --reason 'switch to office left DNS broken'
./macswitcher status
./macswitcher proxy set
./macswitcher proxy unset
./macswitcher proxy detect-auth --proxy '<proxy-host>:<port>'
./macswitcher service install
./macswitcher service start
./macswitcher service restart
./macswitcher service status
./macswitcher observe
```

Legacy aggregate configuration files are read for migration; the next write
stores contexts under `contexts/`.

### What `switch` actually does

`macswitcher switch <context>` runs, in order:

0. **Preflight** — resolve the names the switch depends on, before anything is
   changed. See [Preflight](#preflight) below.
1. Persist the selected context as current (survives even if later steps warn/fail).
2. `scselect` the context's macOS network location, if set (warns, doesn't abort, on failure).
3. Run the context's app/VPN hooks, while the machine can still resolve names
   through the network it is currently on.
4. Rewrite the selected DNS backend's upstreams from the context's
   `upstreams`, if any — AdGuard Home's `upstream_dns_file`, or unbound's
   `forwarders_file`, whichever `dns.backend` selects — and restart only that
   backend, via `applications.adguardhome.restart` or
   `applications.unbound.restart` respectively (warns if that's not
   configured — a stale upstreams load is a common source of "it takes
   forever after switching" symptoms). The other backend, even if fully
   configured, is never opened or restarted by this step.
5. Apply the context's `protection_enabled`, if set and `dns.backend` is
   `adguard` (unbound has no filtering API, so this step is skipped
   entirely when `dns.backend` is `unbound`) — after the restart above,
   which would otherwise undo it, and before the DNS check below, because on a
   corporate network filtering is exactly what stops DNS from working.
6. Point the network services' DNS servers at `dns.local_resolver` (or the
   context's `dns.resolvers` override).
7. Flush the system DNS cache (`dscacheutil -flushcache` +
   `killall -HUP mDNSResponder`, both via `sudo -n` — see below).
8. **Verify DNS actually works** before touching the proxy: resolve
   `google.com` for `off`/`direct` modes, or the forward proxy's own
   `proxy_server` hostname for `forward` mode. If this fails, `switch` stops
   here with an error and a hint to fix DNS and rerun — none of the
   remaining steps (proxy service, app sync) can work with broken DNS
   anyway.
9. Stop (if `proxy_mode: off`) or restart (otherwise) the Alpaca service,
   then set or unset the local proxy accordingly.

Steps 7 and 8 shell out to `sudo -n ...` (non-interactive), so they need
matching passwordless-sudo sudoers entries, e.g.:

```
your-user ALL=(root) NOPASSWD: /usr/bin/dscacheutil -flushcache
your-user ALL=(root) NOPASSWD: /usr/bin/killall -HUP mDNSResponder
your-user ALL=(root) NOPASSWD: /bin/launchctl kickstart -k system/com.macswitcher.adguardhome
```

The last line's target depends on `dns.backend`: it restarts whichever
backend's `applications.<name>.restart` command is actually configured — the
LaunchDaemon label used by AdGuard Home, or, for `dns.backend: unbound`,
unbound's own (e.g. `system/net.unbound`).

Without these, steps 4/7 just print a warning and `switch` continues; step 8
will then fail fast if DNS genuinely isn't working yet.

### Preflight

Step 3 above starts the VPN *before* the resolvers are repointed, and that
ordering is deliberate: the tunnel has to resolve its own gateway through the
network the machine is still on. The consequence is that the whole switch
depends on the resolvers that are configured *right now* working.

When they do not, the failure is a dead end rather than a slow path.
`openconnect` fails with `getaddrinfo failed`, the switch stops partway
through, and every subsequent attempt fails identically. The machine cannot
switch its way out.

Preflight asks the question first. It resolves:

- `dns.check_host`, if the context sets one;
- the forward proxy's own hostname, in `forward` mode;
- everything in the context's `preflight_hosts`.

If any of them fail, it hands **every** network service back to DHCP, tries
once more, and then restores the resolvers exactly as they were — always,
including on `Ctrl-C`. That second answer is the diagnosis:

| second attempt | meaning | fix |
| --- | --- | --- |
| resolves with DHCP | the network is fine, the DNS configuration is not | fix the local resolver (AdGuard Home, `macswitcher observe`), or switch to a context this network can reach |
| still fails | the problem is upstream of DNS configuration | the link itself: Wi-Fi/Ethernet, a captive portal, VPN reachability |

The DHCP attempt is a *diagnosis*, never a repair — leaving the machine on
DHCP would be a third state that is neither the old context nor the new one.
So a context that only passes via DHCP is still a failed switch; it just fails
with something you can act on.

A context that declares none of the three names is not checked. `google.com`
is a fine default for "did the switch work" *afterwards*, but as a
precondition it would refuse to switch on any network that resolves only its
own intranet.

`preflight_hosts` is where the VPN gateway belongs:

```yaml
contexts:
  work:
    preflight_hosts:
      - vpn.corp.example
```

macswitcher does not derive that name itself. On this setup it lives in
`~/.config/corp-vpn/corp-vpn.conf`, read by a shell script — an employer's
layout, which a general tool has no business knowing. As data in a context it
costs one line.

`macswitcher preflight <context>` runs exactly the same check, including the
DHCP fallback, without switching. That is the point of it: you reach for it
when a switch has *already* failed and you want the diagnosis without
triggering the failure again.

### When a switch fails

Earlier versions rolled back: on any failed step the previous context was
re-applied. That was removed, because it does not help and costs the evidence.

A switch fails *because* the machine is on a network the target context does
not match. The previous context describes a network the machine is not on
either — so in the office, rolling back to `home` restores an equally dead
setup, having spent the failure on the way. Two broken states instead of one,
and the second is harder to reason about because half of it was applied twice.

What replaces it is a record of exactly how far the switch got.

**Every switch is transcribed** to `~/Library/Logs/macswitcher-switch.log`, one
self-contained block per switch, headed:

```
=== 2026-09-04T14:31:02+02:00  switch home-vpn -> office  FAILED (18.4s)
```

so `grep FAILED ~/Library/Logs/macswitcher-switch.log` answers "what happened,
when" on its own. Below the header is the step journal — every step with `ok`,
`skipped` or `FAILED` — followed by the full terminal output. `switch` is a
short-lived command, so it never holds an fd across a `newsyslog` rename and
needs no special handling for rotation.

**A failed switch writes a snapshot automatically.** The moment a diagnostic is
needed is the moment there is no network to look up how to ask for one.

### Snapshot

```sh
macswitcher snapshot --reason 'office switch left DNS broken'
```

Collects the machine's whole network and daemon state into
`~/Library/Logs/macswitcher-snapshots/<timestamp>/`:

| path | contents |
| --- | --- |
| `report.md` | the summary — read this first; stands on its own |
| `meta.txt` | version, current context, from → to, reason |
| `switch.log` | the switch that just failed, if the snapshot came from one |
| `switch-history.log` | the five switches before it — first failure or fourth? |
| `dns-probes.txt` | each preflight/check name, resolved or not |
| `daemons/<name>.status.txt` | full state, pid, runs, last exit, detail |
| `daemons/<name>.log.txt` | last 300 lines of that daemon's log |
| `network/scutil-dns.txt` | the resolvers *actually* in effect, per interface |
| `network/per-service.txt` | DNS and proxy settings per network service |
| `network/routes.txt`, `interfaces.txt`, `listening-ports.txt` | the rest |
| `config/` | config, all contexts, generated `filter.pac` and zsh proxy rc |
| `system/uptime.txt`, `sw_vers.txt` | did this machine just wake up? |

Three properties it is built around:

- **It never mutates anything.** A diagnostic that changes things is one nobody
  dares run at the moment it is needed.
- **It never blocks for long.** Every command gets 5 seconds, not the usual 30
  — twenty commands against a broken network would otherwise take ten minutes,
  and a snapshot nobody waits for is a snapshot nobody takes.
- **It never fails as a whole.** A collector that fails records its error as
  its own content. A partial snapshot is worth a great deal; an aborted one
  nothing.

A directory rather than one file, because a snapshot is a few KB of summary and
several hundred KB of raw output — flattened into a single file the summary
drowns under `netstat -rn`. It is still one unit to share:

```sh
tar czf ~/Desktop/snap.tgz -C ~/Library/Logs/macswitcher-snapshots <timestamp>
```

Credentials embedded in URLs (`scheme://user:pass@host`) are redacted, since a
snapshot exists to be sent to somebody. The 20 most recent are kept; `newsyslog`
rotates files, not directories, so the command that creates them prunes them.

`network/scutil-dns.txt` is usually the most useful file in there. It shows the
resolvers in effect including ones a VPN installed, which `networksetup` does
not know about — and a half-finished switch most often leaves resolvers from
one context next to a proxy from the other.

### AdGuard Home filtering per context

A context may carry `protection_enabled`, which turns AdGuard Home's filtering
on or off as part of the switch. Leave it out and macswitcher does not touch
filtering at all — the setting is three-state on purpose, so contexts written
before it existed keep whatever the machine already had rather than silently
losing their filtering.

It exists because a corporate network puts the machine in a bootstrap
deadlock. AdGuard Home wants to fetch its filter lists; that needs the proxy;
the proxy needs DNS; and DNS *is* AdGuard Home. The office context came up with
no working resolver at all, and the only way out was turning filtering off by
hand in the web UI. Filtering there is redundant anyway — everything is
forwarded to corporate systems that filter in their own right:

```yaml
# contexts/office.yaml
proxy_mode: forward
protection_enabled: false
```

This goes through AdGuard Home's HTTP API (`POST /control/protection`), not
through `AdGuardHome.yaml`. That file belongs to the runtime — AdGuard Home
rewrites it wholesale on every web-UI change and it sits in a `0700`
root-owned prefix, so editing it would need `sudo` on every switch and would
race the daemon. The API applies the change to the running process
immediately, needs no restart (so the DNS cache survives), and AdGuard Home
persists it itself.

The call authenticates with an account from the System keychain, defaulting to
the API account the Ansible role seeds rather than the human's web-UI login.
Override any of it under `adguard:`:

```yaml
adguard:
  upstreams_file: /etc/adguardhome/upstreams.conf
  address: 127.0.0.3:3053
  api_user: zonesync
  api_keychain_service: adguardhome api
```

A failure here warns but never aborts a switch: failing to *disable* filtering
breaks name resolution, which the DNS check catches moments later with a hint
naming this setting, and failing to *enable* it costs filtering rather than
connectivity — taking the network down over that would be the wrong trade.
Neither passes unnoticed, because `macswitcher status` reports the requested
state next to the daemon's live one, and `observe` calls out filtering that is
off.

## Observe

`macswitcher observe` opens a terminal UI (built with
[Bubble Tea](https://github.com/charmbracelet/bubbletea)) showing the live
status of every daemon macswitcher cares about, refreshed automatically every
5 seconds:

- **alpaca** — macswitcher's own launch agent (`com.schretzi.macswitcher`,
  installed by `service install`). Shows running/stopped, pid, uptime, the
  launchd restart counter (`runs`), and the active context's `proxy_mode`
  plus, in `forward` mode, the upstream host:port and whether auth is
  Keychain- or Kerberos-based.
- **adguardhome** — shows running/stopped, the default upstreams currently
  in `adguard.upstreams_file`, how many per-domain overrides are preserved
  alongside them, and calls out filtering that is off. Only what a switch
  actually rewrites when `dns.backend` is `adguard`.
- **unbound** — shows running/stopped and the `forward-addr` entries
  currently in `unbound.forwarders_file`. Only what a switch actually
  rewrites when `dns.backend` is `unbound`; while `dns.backend` is `adguard`
  the row still shows unbound's current forwarders (from whichever switch
  last had `dns.backend: unbound`), noting that a switch does not rewrite
  them.
- **kerberoskeepalive** — shows running/stopped and, for the `ccache_path` of
  `KerberosKeepAlive`'s first profile, whether `klist` reports a valid,
  non-expired ticket. An invalid ticket is reported as survivable, because
  alpaca falls back to Basic against the forward proxy.
- **omt** — shown only if the `omt` binary is on `PATH`; runs `omt status`
  and summarizes how many configured OAuth2 accounts have a valid token.
- **vpn** — a generic LaunchAgent-supervised VPN connection (e.g. an
  `openconnect` wrapper script). Shows running/stopped, and, if
  `daemons.vpn.interface` is set, whether that tunnel interface currently
  has an address (a running supervisor process doesn't guarantee the
  tunnel actually came up).
- **tunneling** — shown only if the `tunneling` binary is on `PATH`; runs
  `tunneling status` and summarizes how many of the configured SSH/GCP-IAP
  tunnels have an open local port. Listed last because its tunnels ride on
  whatever the rows above set up, so a failure here is usually a symptom of
  one of them.

AdGuard Home, unbound, KerberosKeepAlive, omt, vpn, and tunneling are not
installed by macswitcher (they come from Homebrew or an external Ansible
role/script), so their launchd labels must be configured explicitly:

```yaml
daemons:
  adguardhome:
    label: com.macswitcher.adguardhome
  unbound:
    label: homebrew.mxcl.unbound   # this is the default if omitted
  kerberos_keep_alive:
    label: com.example.kerberoskeepalive
  omt:
    label: com.schretzi.omt
  vpn:
    label: com.example.vpn
    interface: utun99              # optional: enables the tunnel-up check
  tunneling:
    label: com.schretzi.tunneling
```

Any daemon whose `label` is empty (the default for `kerberos_keep_alive`,
`omt`, `vpn`, and `tunneling`) is shown as "not configured" and cannot be
controlled from the TUI.
`macswitcher config validate` checks the `daemons:` block for unrecognized
keys or fields (a common source of silent typos, since unknown YAML keys are
otherwise just ignored).

By default each daemon is assumed to be a per-user LaunchAgent, loaded in the
`gui/<uid>` domain from `~/Library/LaunchAgents/<label>.plist`. Some daemons
— notably a system-wide unbound install — instead run as a LaunchDaemon in
the `system` domain from `/Library/LaunchDaemons/<label>.plist`. Set
`scope: system` for those:

```yaml
daemons:
  unbound:
    label: net.unbound
    scope: system
```

Inspecting a system-scoped daemon's status never needs elevated privileges,
but starting, stopping, restarting, enabling, or disabling one does: those
actions run `sudo launchctl ...` interactively, suspending the TUI and
handing the real terminal to `sudo` so it can prompt for your password, then
resuming once it exits.

Keys: `↑`/`↓` or `j`/`k` to select a row, `s` start, `h` halt, `R` restart,
`e` enable, `d` disable, `l` logs, `S` switch context, `r` to refresh
immediately, `q`/`Esc`/`Ctrl-C` to quit. Start/halt map to `launchctl
bootstrap`/`bootout` (load state right now); enable/disable map to `launchctl
enable`/`disable` (a persisted override independent of whether the agent is
currently loaded, so a disabled agent stays off across reboots even with
`RunAtLoad` set).

### Halting a daemon the network depends on

`alpaca`, `adguardhome` and `privoxy` carry this machine's DNS and outbound
HTTP. Halting or disabling one of them does not degrade the setup, it
disconnects the machine — including the TUI's own ability to report what
happened. Because the keymap is single-key and unmodified, and the cursor
starts on `alpaca`, one stray keystroke used to be enough to do it.

So `h` and `d` on those three rows ask first: the status line names the
daemon and what specifically breaks, and only a literal `y` proceeds. Any
other key cancels — including `Esc`, which does *not* also quit while a
confirmation is on screen. `s`, `R` and `e` never ask; they all end with the
daemon running or runnable.

### Switching context from the TUI

`S` opens a modal listing every configured context, with the cursor on — and
the name of — the active one. `Enter` switches to the selected context, `Esc`
cancels.

The switch runs as a child `macswitcher switch <context>` process and its
output is streamed into the modal, so what you read there is byte for byte
what the command prints on a terminal — and the same text is appended to
`~/Library/Logs/macswitcher-switch.log`, so closing the modal does not lose
it. While it runs, every key is ignored: a context switch rewrites DNS, the
proxy and several daemons in sequence, and interrupting it halfway leaves the
machine in a state nothing has recorded a reason for. Once it finishes,
`Enter` or `Esc` closes the modal and the list behind it is reloaded from the
rewritten config.


## Development

```sh
lefthook install       # one-time per clone: enables the gitleaks pre-commit hook
make pipeline          # fmt, lint, security (govulncheck + gosec), test, build
make docs              # regenerate command docs under docs/
```

CI (`.github/workflows/ci.yml`) mirrors `make pipeline` with separate `test`,
`lint`, `security`, and `build` jobs on every push and pull request to `main`.

### Releasing

Releases are built with [goreleaser](https://goreleaser.com) and are
manual-only, never automatic on tag push:

```sh
git tag vX.Y.Z
git push origin vX.Y.Z
```

Then go to Actions → Release → Run workflow, selecting that tag. This builds
darwin binaries, publishes a GitHub release with checksums and per-archive
[SPDX SBOMs](https://spdx.dev) (generated by [syft](https://github.com/anchore/syft)),
and updates the Homebrew cask in
[schretzi/homebrew-tap](https://github.com/schretzi/homebrew-tap) (requires
a `HOMEBREW_TAP_TOKEN` repo secret with write access to that tap repo).

## AI usage

Parts of this project's scaffolding (CI pipeline, goreleaser config, Makefile,
lefthook/gitleaks hook, and Cobra doc generator) were added with GitHub
Copilot CLI. Architecture and tool choices (GitHub Actions, goreleaser, gitleaks, Homebrew tap)
were decided jointly after researching the actual behavior of each tool
locally; the generated configuration was verified end-to-end by running the
local pipeline (`make pipeline`), a goreleaser snapshot build, and a live
gitleaks pre-commit test, rather than assumed to work.
