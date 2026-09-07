#!/usr/bin/env bash
# Shared helpers for the onboarding install scripts. Not meant to be run
# directly; sourced by unbound-alpaca/install.sh and adguard-alpaca/install.sh.
#
# Design goals, since these replace an Ansible role:
# - idempotent: safe to re-run after editing a config file.
# - never silently clobbers something a human may have edited: existing files
#   are backed up with a timestamp suffix before being overwritten.
# - fails loudly and early (`set -euo pipefail` in the callers) rather than
#   leaving a half-applied setup that looks done.

onboarding_require_macos() {
  if [[ "$(uname -s)" != "Darwin" ]]; then
    echo "error: this only runs on macOS" >&2
    exit 1
  fi
}

onboarding_require_brew() {
  if ! command -v brew >/dev/null 2>&1; then
    echo "error: Homebrew is required (https://brew.sh) and was not found on PATH" >&2
    exit 1
  fi
}

# onboarding_brew_install <formula-or-cask...>
# Installs anything not already installed. Skips already-installed formulae
# entirely rather than upgrading them - this script installs a working setup,
# it does not manage your Homebrew upgrade cadence.
onboarding_brew_install() {
  local pkg
  for pkg in "$@"; do
    if brew list --formula "$pkg" >/dev/null 2>&1 || brew list --cask "$pkg" >/dev/null 2>&1; then
      echo "brew: $pkg already installed"
    else
      echo "brew: installing $pkg"
      brew install "$pkg"
    fi
  done
}

# onboarding_backup_if_exists <path>
# Renames an existing file to <path>.bak.<timestamp> so a re-run never loses
# something you hand-edited without a trace.
onboarding_backup_if_exists() {
  local path="$1"
  if [[ -e "$path" && ! -L "$path" ]]; then
  local backup
  backup="${path}.bak.$(date +%Y%m%dT%H%M%S)"
    echo "backing up existing $path -> $backup"
    cp -p "$path" "$backup"
  fi
}

# onboarding_install_file <src> <dst> [mode]
# Copies src to dst, creating parent dirs, backing up any existing dst first.
# Mode defaults to 0644.
onboarding_install_file() {
  local src="$1" dst="$2" mode="${3:-0644}"
  mkdir -p "$(dirname "$dst")"
  onboarding_backup_if_exists "$dst"
  cp "$src" "$dst"
  chmod "$mode" "$dst"
  echo "wrote $dst"
}

# onboarding_render_template <src-template> <dst> [mode]
# Like onboarding_install_file, but expands @VAR@-style placeholders from the
# environment first (envsubst-style, but restricted to a known placeholder
# list so a stray '@' in a real hostname/URL is never touched by accident).
#
# ONBOARDING_TEMPLATE_VARS must be set by the caller to a space-separated
# list of variable names to substitute, e.g.:
#   ONBOARDING_TEMPLATE_VARS="CORP_PROXY_HOST CORP_PROXY_PORT" \
#     onboarding_render_template config.yaml.tmpl "$dst"
onboarding_render_template() {
  local src="$1" dst="$2" mode="${3:-0644}"
  local tmp
  tmp="$(mktemp)"
  cp "$src" "$tmp"
  local var
  for var in ${ONBOARDING_TEMPLATE_VARS:-}; do
    local value="${!var:-}"
    # Use a delimiter unlikely to appear in a hostname/URL/path.
    python3 - "$tmp" "@${var}@" "$value" <<'PYEOF'
import sys
path, placeholder, value = sys.argv[1], sys.argv[2], sys.argv[3]
with open(path, "r", encoding="utf-8") as f:
    content = f.read()
content = content.replace(placeholder, value)
with open(path, "w", encoding="utf-8") as f:
    f.write(content)
PYEOF
  done
  mkdir -p "$(dirname "$dst")"
  onboarding_backup_if_exists "$dst"
  mv "$tmp" "$dst"
  chmod "$mode" "$dst"
  echo "wrote $dst"
}

# onboarding_write_sudoers_rule <rule-file-name> <content>
# Installs a validated sudoers snippet under /etc/sudoers.d/. Uses `visudo
# -cf` to check syntax BEFORE installing, so a typo cannot lock out sudo.
onboarding_write_sudoers_rule() {
  local name="$1" content="$2"
  local tmp dst="/etc/sudoers.d/${name}"
  tmp="$(mktemp)"
  printf '%s\n' "$content" >"$tmp"
  chmod 0440 "$tmp"
  if ! visudo -cf "$tmp" >/dev/null; then
    echo "error: generated sudoers rule for '$name' failed validation, not installing" >&2
    rm -f "$tmp"
    return 1
  fi
  if [[ -f "$dst" ]] && cmp -s "$tmp" "$dst"; then
    echo "sudoers: $dst already up to date"
    rm -f "$tmp"
    return 0
  fi
  echo "sudoers: installing $dst (requires sudo)"
  sudo install -o root -g wheel -m 0440 "$tmp" "$dst"
  rm -f "$tmp"
}

# onboarding_current_user - the login user these scripts install things for,
# even when invoked via sudo for a single step.
onboarding_current_user() {
  echo "${SUDO_USER:-$(id -un)}"
}

# onboarding_install_loopback_alias <label> <ip>
# macOS does not persist extra lo0 aliases across reboot, and mDNSResponder
# already owns 127.0.0.1:53 - binding a resolver there fails or fights with
# it. A dedicated alias avoids both problems. This installs a LaunchDaemon
# that runs `ifconfig lo0 alias <ip> up` once at boot (RunAtLoad, no
# KeepAlive - it's a one-shot command, not a long-running process), and runs
# it immediately so the current boot has the alias too.
onboarding_install_loopback_alias() {
  local label="$1" ip="$2"
  local plist="/Library/LaunchDaemons/${label}.plist"
  local tmp
  tmp="$(mktemp)"
  cat >"$tmp" <<PLIST
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key>
  <string>${label}</string>
  <key>ProgramArguments</key>
  <array>
    <string>/sbin/ifconfig</string>
    <string>lo0</string>
    <string>alias</string>
    <string>${ip}</string>
    <string>up</string>
  </array>
  <key>RunAtLoad</key>
  <true/>
</dict>
</plist>
PLIST
  if [[ -f "$plist" ]] && cmp -s "$tmp" "$plist"; then
    echo "loopback alias: $plist already up to date"
  else
    echo "loopback alias: installing $plist (requires sudo)"
    sudo install -o root -g wheel -m 0644 "$tmp" "$plist"
    sudo launchctl bootout system "$plist" >/dev/null 2>&1 || true
    sudo launchctl bootstrap system "$plist"
  fi
  rm -f "$tmp"
  # Idempotent for the current boot too: adding an alias that already exists
  # is a harmless no-op, so this is safe to run every time.
  sudo ifconfig lo0 alias "$ip" up
}
