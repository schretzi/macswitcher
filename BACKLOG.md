# Backlog

Open topics for macswitcher, kept here so they survive between sessions.

## Open

- [ ] **Sort out which files are config, which are state, and what
      `config init` is allowed to overwrite.**

      This surfaced while symlinking a context file out of a dotfiles
      repository instead of copying it in. The rule that had been applied to
      macswitcher's files — seed them, never link them, because "macswitcher
      rewrites them in place on every switch" — turns out not to hold for the
      context files, and the difference matters to anyone managing these files
      with Ansible, chezmoi or a plain stow.

      What `internal/app` does today:

      | file | written when | by |
      | ---- | ------------ | -- |
      | `~/.config/macswitcher/config.yaml` | `config init` only | `config.go:484` |
      | `~/.config/macswitcher/contexts/*.yaml` | `config init` only | `config.go:496` |
      | `~/.config/macswitcher/state.json` | every switch | `config.go:318` |
      | `~/.zsh/rcs/proxy` | every switch | `system.go:59` |
      | `~/.docker/config.json` | every switch | `system.go:112` |
      | alpaca proxy conf | every switch | `proxy.go:352` |
      | unbound `forwarders.conf` | every switch | `network.go:339` |
      | AdGuard Home upstreams file | every switch | `network.go:275` |

      Four things to decide:

      - **`state.json` is in the config directory.** `statePath()` puts it
        next to `config.yaml`. Machine-written state that the process
        republishes belongs in `~/.local/state/macswitcher/` (MacbookSetup's
        `CONVENTIONS.md` §2a). Moving it leaves `~/.config/macswitcher/`
        purely human-owned, which is what makes the seed-vs-link question go
        away entirely rather than needing a rule.
      - **`saveRuntimeState` meets neither state requirement.** It is a plain
        `os.WriteFile`: not atomic, so a reader can observe a half-written
        file (should be temp file in the same directory, then `rename`), and
        it carries no PID, so state left behind by a dead process is
        indistinguishable from a live one. `tunneling` already does both —
        worth copying rather than reinventing.
      - **Nothing is backed up before it is overwritten.** Every writer above
        is a bare `os.WriteFile`. For the derived files (`forwarders.conf`,
        the AdGuard upstreams, `~/.zsh/rcs/proxy`) that is correct — they are
        regenerated from the config. For anything a human may have edited it
        is not.
      - **`config init` guards on the wrong path.** It refuses to run when
        `config.yaml` exists, then writes `config.yaml` *and every context
        file*. A missing `config.yaml` next to a populated `contexts/`
        therefore overwrites the contexts without warning — and because they
        are marshalled from the struct, every comment in them is lost. If a
        context is a symlink, the write follows it into whatever repository
        it points at. Options: widen the guard to cover `contexts/`, refuse
        to write through a symlink, or make init strictly additive.
