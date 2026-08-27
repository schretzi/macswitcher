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
proxy_mode: direct
apps:
  reload:
    - unbound
```

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

`proxy_mode` accepts exactly three values:

- `off` — removes all proxy configuration (system network services, the
  `~/.zsh/rcs/proxy` shell env file, and Docker's `~/.docker/config.json`) and
  stops the local proxy service.
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

**Kerberos/Negotiate (ticket file)**:

```yaml
proxy_mode: forward
forwarder_proxy:
  proxy_server: proxy.example.com
  port: 8080
  ticket_file: ~/Library/Caches/tickets/work.krb5cc
  pac_file: https://proxy.example.com/proxy.pac
  auth_allowlist:
    - proxy.example.com
```

- Set `ticket_file` instead of `username`/`password_keychain_service` when the
  upstream proxy authenticates via Kerberos/Negotiate. `username`,
  `password_keychain_service`, and `password_keychain_account` are ignored and
  not required in this mode — there is no Keychain entry to create, and
  `proxy password-set` refuses to run against a `ticket_file` context.
  `pac_file` and `auth_allowlist` behave exactly as in the NTLM/Basic path.
- `ticket_file` points at the Kerberos credential cache (ccache) file. It is
  not managed by macswitcher: a separate `KerberosKeepAlive` launch agent is
  expected to run continuously in the background, renewing/refreshing the
  ticket in that file. macswitcher's job is only to hand that already-alive
  ticket to Alpaca, not to acquire or renew it.
- At `proxy set`/`proxy` runtime, macswitcher exports `KRB5CCNAME` pointing at
  `ticket_file` (expanding a leading `~/`) for the Alpaca process, and also
  exposes `{{ticket_file}}` and `{{upstream_proxy}}` (`proxy_server:port`) as
  template tokens for a context/global `alpaca.command`, for Alpaca builds
  that take the ccache path or upstream proxy as explicit flags instead of
  via environment. `{{cntlm_conf}}` and `{{upstream_url}}` are NTLM/Basic-only
  and error out if the active `forwarder_proxy` has `ticket_file` set.

The public config contains no environment-specific IPs, hostnames, usernames,
PAC URLs, or proxy credentials. Put those values in the private/work overlays.
Secrets (Keychain passwords and Kerberos ticket files) are never configuration
file values.

For a machine not managed by Ansible, `macswitcher config init` creates a
`home` context from the current macOS network location, network services, DNS
resolver, and readable Unbound forwarders, plus an intentionally empty `work`
context.

Unbound forwarders are managed at
`/opt/homebrew/etc/unbound/conf.d/forwarders.conf`. Ansible initially links
this path to its default `Dotfiles/unbound/forwarders.conf`. Before writing
context-specific forwarders, macswitcher removes only that symlink and creates
a real runtime-managed file, so the repository source is never modified.

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
./macswitcher status
./macswitcher proxy set
./macswitcher proxy unset
./macswitcher proxy detect-auth --proxy '<proxy-host>:<port>'
./macswitcher service install
./macswitcher service start
./macswitcher service restart
./macswitcher service status
```

Legacy aggregate configuration files are read for migration; the next write
stores contexts under `contexts/`.

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

