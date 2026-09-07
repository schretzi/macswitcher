#!/usr/bin/env bash
# Installs the "Unbound + Alpaca" minimal macswitcher setup: no AdGuard Home,
# no Privoxy, no Kerberos daemon. Unbound resolver (its default upstreams
# rewritten by macswitcher on every switch, dns.backend: unbound) + alpaca
# forward proxy, switching between "home" (direct) and "office" (forward)
# contexts.
#
# Idempotent: re-run after editing config.yaml/contexts/*.yaml/unbound.conf to
# apply changes. Existing files are backed up with a timestamp suffix, never
# silently overwritten.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=../lib/common.sh
source "${SCRIPT_DIR}/../lib/common.sh"

onboarding_require_macos
onboarding_require_brew

echo "== 1/7: installing macswitcher and alpaca =="
onboarding_brew_install schretzi/tap/macswitcher samuong/alpaca/alpaca unbound

BREW_PREFIX="$(brew --prefix)"
UNBOUND_CONF="${BREW_PREFIX}/etc/unbound/unbound.conf"
UNBOUND_FORWARDERS="${BREW_PREFIX}/etc/unbound/conf.d/forwarders.conf"
MACSWITCHER_CFG="${HOME}/.config/macswitcher/config.yaml"
MACSWITCHER_CTX_DIR="${HOME}/.config/macswitcher/contexts"

echo "== 2/7: creating the 127.0.0.2 loopback alias (127.0.0.1:53 is taken by mDNSResponder) =="
onboarding_install_loopback_alias "com.macswitcher.onboarding.localhost-alias" "127.0.0.2"

echo "== 3/7: installing Unbound config (permanent forward-zones + include of the file macswitcher rewrites) =="
onboarding_install_file "${SCRIPT_DIR}/unbound.conf" "${UNBOUND_CONF}" 0644
if grep -q 'CHANGE-ME' "${UNBOUND_CONF}"; then
  echo "warning: ${UNBOUND_CONF} still has CHANGE-ME placeholders - edit it," \
    "then re-run this script (or 'sudo brew services restart unbound')" >&2
fi

echo "== 4/7: seeding the forwarders file unbound.conf includes, so unbound has something to load before the first switch =="
if [[ ! -f "${UNBOUND_FORWARDERS}" ]]; then
  onboarding_backup_if_exists "${UNBOUND_FORWARDERS}"
  mkdir -p "$(dirname "${UNBOUND_FORWARDERS}")"
  printf 'forward-zone:\n  name: "."\n  forward-addr: 9.9.9.9\n  forward-addr: 1.1.1.1\n' > "${UNBOUND_FORWARDERS}"
  chmod 0644 "${UNBOUND_FORWARDERS}"
fi

echo "== 5/7: starting Unbound as a system service (needs sudo to bind port 53) =="
sudo brew services start unbound

echo "== 6/7: installing macswitcher config and contexts =="
onboarding_install_file "${SCRIPT_DIR}/config.yaml" "${MACSWITCHER_CFG}" 0644
onboarding_install_file "${SCRIPT_DIR}/contexts/home.yaml" "${MACSWITCHER_CTX_DIR}/home.yaml" 0644
onboarding_install_file "${SCRIPT_DIR}/contexts/office.yaml" "${MACSWITCHER_CTX_DIR}/office.yaml" 0644
if grep -q 'CHANGE-ME' "${MACSWITCHER_CTX_DIR}/office.yaml"; then
  echo "warning: ${MACSWITCHER_CTX_DIR}/office.yaml still has CHANGE-ME placeholders" \
    "- edit it before 'macswitcher switch office'" >&2
fi

echo "== 7/7: sudoers rules (DNS cache flush + unbound restart, both needed on every switch) =="
CURRENT_USER="$(onboarding_current_user)"
onboarding_write_sudoers_rule "macswitcher-dns-flush" \
  "${CURRENT_USER} ALL=(root) NOPASSWD: /usr/bin/dscacheutil -flushcache
${CURRENT_USER} ALL=(root) NOPASSWD: /usr/bin/killall -HUP mDNSResponder"
onboarding_write_sudoers_rule "macswitcher-unbound-restart" \
  "${CURRENT_USER} ALL=(root) NOPASSWD: ${BREW_PREFIX}/bin/brew services restart unbound"

echo "== registering the macswitcher LaunchAgent (supervises alpaca) =="
macswitcher service install

cat <<EOF

Done. Next steps:
1. Edit ${UNBOUND_CONF} and ${MACSWITCHER_CTX_DIR}/office.yaml, replacing
   every CHANGE-ME with your employer's actual proxy/DNS values.
2. sudo brew services restart unbound   (after editing unbound.conf)
3. macswitcher proxy password-set       (stores the office proxy password in Keychain)
4. macswitcher config validate
5. macswitcher switch home
EOF

