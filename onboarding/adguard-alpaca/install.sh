#!/usr/bin/env bash
# Installs the "AdGuard Home + Alpaca" minimal macswitcher setup: no
# Privoxy, no Kerberos daemon. AdGuard Home (per-context upstream
# rewriting) + alpaca forward proxy, switching between "home" (direct) and
# "office" (forward) contexts.
#
# Idempotent: re-run after editing config.yaml/contexts/*.yaml/upstreams.conf
# to apply changes. AdGuard Home's own AdGuardHome.yaml is only ever seeded
# ONCE (matching how AdGuard Home itself owns that file after first start -
# see the comment above the bootstrap step below); everything else is backed
# up with a timestamp suffix before being overwritten.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=../lib/common.sh
source "${SCRIPT_DIR}/../lib/common.sh"

onboarding_require_macos
onboarding_require_brew

ADGUARD_LABEL="com.macswitcher.onboarding.adguardhome"
ADGUARD_IP="127.0.0.2"
ADGUARD_DNS_PORT=53
ADGUARD_WEB_PORT=3053
ADGUARD_DIR="/opt/adguardhome"
ADGUARD_BIN="${ADGUARD_DIR}/AdGuardHome"
ADGUARD_YAML="${ADGUARD_DIR}/AdGuardHome.yaml"
ADGUARD_ETC="/etc/adguardhome"
UPSTREAMS_FILE="${ADGUARD_ETC}/upstreams.conf"

echo "== 1/8: installing macswitcher and alpaca =="
onboarding_brew_install schretzi/tap/macswitcher samuong/alpaca/alpaca

echo "== 2/8: creating the ${ADGUARD_IP} loopback alias (127.0.0.1:53 is taken by mDNSResponder) =="
onboarding_install_loopback_alias "com.macswitcher.onboarding.localhost-alias" "${ADGUARD_IP}"

echo "== 3/8: downloading AdGuard Home =="
if [[ -x "${ADGUARD_BIN}" ]]; then
  echo "AdGuard Home already installed at ${ADGUARD_BIN}"
else
  ARCH="$(uname -m)"
  case "$ARCH" in
    arm64) AG_ARCH="darwin_arm64" ;;
    x86_64) AG_ARCH="darwin_amd64" ;;
    *) echo "error: unsupported architecture $ARCH" >&2; exit 1 ;;
  esac
  RELEASE_URL="$(curl -fsSL https://api.github.com/repos/AdguardTeam/AdGuardHome/releases/latest \
    | grep -o "\"browser_download_url\": *\"[^\"]*${AG_ARCH}\\.zip\"" \
    | head -1 | cut -d'"' -f4)"
  if [[ -z "$RELEASE_URL" ]]; then
    echo "error: could not find an AdGuard Home release for ${AG_ARCH}" >&2
    exit 1
  fi
  echo "downloading $RELEASE_URL"
  TMPZIP="$(mktemp).zip"
  curl -fsSL -o "$TMPZIP" "$RELEASE_URL"
  TMPDIR="$(mktemp -d)"
  unzip -q "$TMPZIP" -d "$TMPDIR"
  sudo mkdir -p "${ADGUARD_DIR}" "${ADGUARD_DIR}/data" "${ADGUARD_ETC}" "${ADGUARD_ETC}/zones"
  sudo install -o root -g wheel -m 0755 "${TMPDIR}/AdGuardHome/AdGuardHome" "${ADGUARD_BIN}"
  sudo chown root:wheel "${ADGUARD_DIR}" "${ADGUARD_DIR}/data"
  rm -rf "$TMPZIP" "$TMPDIR"
fi

echo "== 4/8: seeding the per-domain upstreams file (only if it doesn't already exist) =="
if [[ -f "${UPSTREAMS_FILE}" ]]; then
  echo "${UPSTREAMS_FILE} already exists, leaving it alone (macswitcher owns its default block; edit per-domain lines by hand)"
else
  sudo install -o root -g admin -m 0664 "${SCRIPT_DIR}/upstreams.conf" "${UPSTREAMS_FILE}"
  echo "wrote ${UPSTREAMS_FILE} (group-writable by 'admin' so macswitcher can rewrite it without sudo)"
fi
if grep -q 'CHANGE-ME' "${UPSTREAMS_FILE}"; then
  echo "warning: ${UPSTREAMS_FILE} still has a CHANGE-ME placeholder for your VPN gateway hostname" >&2
fi

echo "== 5/8: first-run setup (only if AdGuard Home has never been configured) =="
if [[ -f "${ADGUARD_YAML}" ]]; then
  echo "${ADGUARD_YAML} already exists, skipping first-run setup"
else
  # AdGuard Home writes its own AdGuardHome.yaml, in whatever schema matches
  # the version just downloaded - safer than hand-crafting one here that
  # would silently drift out of sync with a future release. This uses its
  # documented headless bootstrap: run it once with no config, POST the
  # desired addresses + admin credentials to its own /control/install
  # API, then let it become the real, permanently-running instance.
  ADMIN_PASSWORD="$(openssl rand -base64 24)"
  echo "starting AdGuard Home in setup mode..."
  # shellcheck disable=SC2024 # intentional: log file is owned by the invoking user, not root
  sudo "${ADGUARD_BIN}" --work-dir "${ADGUARD_DIR}/data" --config "${ADGUARD_YAML}" \
    --web-addr "${ADGUARD_IP}:${ADGUARD_WEB_PORT}" >/tmp/adguardhome-setup.log 2>&1 &
  SETUP_PID=$!
  for _ in $(seq 1 30); do
    if curl -fsS "http://${ADGUARD_IP}:${ADGUARD_WEB_PORT}/control/status" >/dev/null 2>&1; then
      break
    fi
    sleep 1
  done
  curl -fsS -X POST "http://${ADGUARD_IP}:${ADGUARD_WEB_PORT}/control/install/configure" \
    -H 'Content-Type: application/json' \
    -d "{\"web\":{\"ip\":\"${ADGUARD_IP}\",\"port\":${ADGUARD_WEB_PORT}},\"dns\":{\"ip\":\"${ADGUARD_IP}\",\"port\":${ADGUARD_DNS_PORT}},\"username\":\"admin\",\"password\":\"${ADMIN_PASSWORD}\"}" \
    || {
      echo "error: automated setup failed - see /tmp/adguardhome-setup.log, or finish manually at http://${ADGUARD_IP}:${ADGUARD_WEB_PORT}" >&2
      kill "$SETUP_PID" 2>/dev/null || true
      exit 1
    }
  sudo kill "$SETUP_PID" 2>/dev/null || true
  sleep 1
  security add-generic-password -U -s "macswitcher-onboarding-adguardhome" -a admin -w "${ADMIN_PASSWORD}"
  echo "AdGuard Home admin account created; password stored in Keychain (service: macswitcher-onboarding-adguardhome)"
fi

echo "== 6/8: registering the AdGuard Home LaunchDaemon =="
ADGUARD_PLIST="/Library/LaunchDaemons/${ADGUARD_LABEL}.plist"
TMP_PLIST="$(mktemp)"
cat >"$TMP_PLIST" <<PLIST
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key>
  <string>${ADGUARD_LABEL}</string>
  <key>ProgramArguments</key>
  <array>
    <string>${ADGUARD_BIN}</string>
    <string>--work-dir</string>
    <string>${ADGUARD_DIR}/data</string>
    <string>--config</string>
    <string>${ADGUARD_YAML}</string>
    <string>--no-check-update</string>
  </array>
  <key>RunAtLoad</key>
  <true/>
  <key>KeepAlive</key>
  <true/>
  <key>StandardErrorPath</key>
  <string>/var/log/adguardhome.err.log</string>
</dict>
</plist>
PLIST
if [[ -f "$ADGUARD_PLIST" ]] && cmp -s "$TMP_PLIST" "$ADGUARD_PLIST"; then
  echo "$ADGUARD_PLIST already up to date"
else
  sudo install -o root -g wheel -m 0644 "$TMP_PLIST" "$ADGUARD_PLIST"
  sudo launchctl bootout system "$ADGUARD_PLIST" >/dev/null 2>&1 || true
  sudo launchctl bootstrap system "$ADGUARD_PLIST"
fi
rm -f "$TMP_PLIST"

echo "== 7/8: installing macswitcher config and contexts =="
MACSWITCHER_CFG="${HOME}/.config/macswitcher/config.yaml"
MACSWITCHER_CTX_DIR="${HOME}/.config/macswitcher/contexts"
onboarding_install_file "${SCRIPT_DIR}/config.yaml" "${MACSWITCHER_CFG}" 0644
onboarding_install_file "${SCRIPT_DIR}/contexts/home.yaml" "${MACSWITCHER_CTX_DIR}/home.yaml" 0644
onboarding_install_file "${SCRIPT_DIR}/contexts/office.yaml" "${MACSWITCHER_CTX_DIR}/office.yaml" 0644
if grep -q 'CHANGE-ME' "${MACSWITCHER_CTX_DIR}/office.yaml"; then
  echo "warning: ${MACSWITCHER_CTX_DIR}/office.yaml still has CHANGE-ME placeholders" \
    "- edit it before 'macswitcher switch office'" >&2
fi

echo "== 8/8: sudoers rules (DNS flush + AdGuard Home restart) =="
CURRENT_USER="$(onboarding_current_user)"
onboarding_write_sudoers_rule "macswitcher-dns-flush" \
  "${CURRENT_USER} ALL=(root) NOPASSWD: /usr/bin/dscacheutil -flushcache
${CURRENT_USER} ALL=(root) NOPASSWD: /usr/bin/killall -HUP mDNSResponder"
onboarding_write_sudoers_rule "macswitcher-onboarding-adguardhome-restart" \
  "${CURRENT_USER} ALL=(root) NOPASSWD: /bin/launchctl kickstart -k system/${ADGUARD_LABEL}"

echo "== registering the macswitcher LaunchAgent (supervises alpaca) =="
macswitcher service install

cat <<EOF

Done. Next steps:
1. Edit ${UPSTREAMS_FILE} and ${MACSWITCHER_CTX_DIR}/office.yaml, replacing
   every CHANGE-ME with your employer's actual proxy/DNS values.
2. macswitcher proxy password-set       (stores the office proxy password in Keychain)
3. macswitcher config validate
4. macswitcher switch home
5. Visit http://${ADGUARD_IP}:${ADGUARD_WEB_PORT} (admin account created above) if you
   ever want to check AdGuard Home's own UI.
EOF
