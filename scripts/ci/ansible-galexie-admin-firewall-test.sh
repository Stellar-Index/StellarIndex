#!/usr/bin/env bash
# ansible-galexie-admin-firewall-test.sh — pins Q011/T622: galexie's
# admin_port setting is a bare int with no bind-host option (upstream
# stellar-galexie internal/app.go newAdminServer does
# `fmt.Sprintf(":%d", adminPort)` — all interfaces). Two template
# comments and one defaults comment used to claim it was "bound
# loopback" / "loopback only", which is false at the process level;
# the actual control is 11-firewall.yml's default-deny, which only
# holds as long as galexie_admin_port / galexie_backfill_admin_port
# stay OUT of both public_allow_ports_base and internal_allow_ports_base.
#
# This test fails red if either port is added to an allow-list (the
# real exposure the old comment hid) or if the false "bound loopback"
# claim reappears next to admin_port.
#
# Structural only (grep over the real files) — no hosts, no ansible run.
set -uo pipefail

cd "$(dirname "$0")/../.." || exit 1

DEFAULTS="configs/ansible/roles/archival-node/defaults/main.yml"
LIVE_TPL="configs/ansible/roles/archival-node/templates/galexie.toml.j2"
BACKFILL_TPL="configs/ansible/roles/archival-node/templates/galexie-backfill.toml.j2"

pass=0; fail=0
ok()  { pass=$((pass + 1)); echo "  ok   — $1"; }
bad() { fail=$((fail + 1)); echo "  FAIL — $1"; }

for f in "$DEFAULTS" "$LIVE_TPL" "$BACKFILL_TPL"; do
  if [ ! -f "$f" ]; then
    echo "ansible-galexie-admin-firewall-test: FAIL — $f not found" >&2
    exit 1
  fi
done

# 1. The false "bound loopback" / "loopback only" claim must not
#    reappear next to admin_port in either template or in defaults.
for f in "$LIVE_TPL" "$BACKFILL_TPL"; do
  # Join the comment block onto one line first — the claim wraps
  # across '# ...' lines in both templates ("(bound\n# loopback)").
  block="$(grep -B3 '^admin_port' "$f" | tr '\n' ' ')"
  if grep -qiE 'bound[[:space:]#]*loopback|loopback[[:space:]#]*only' <<<"$block"; then
    bad "$f: admin_port comment still claims a loopback bind galexie does not have"
  else
    ok "$f: admin_port comment no longer claims a bind galexie does not have"
  fi
done

admin_port_lines="$(grep -E 'galexie_(admin_port|backfill_admin_port):' "$DEFAULTS")"
if grep -qiE 'loopback only' <<<"$admin_port_lines"; then
  bad "$DEFAULTS: galexie admin port default still claims 'loopback only'"
else
  ok "$DEFAULTS: galexie admin port default no longer claims 'loopback only'"
fi

# 2. The ports must stay out of both allow-lists — that absence, under
#    the role's default-deny input policy, is what actually closes
#    them to the outside. yq isn't guaranteed on every runner: grep
#    the two allow-list blocks (bounded by the next top-level key)
#    for the port numbers directly.
allow_list_block() {  # <key>
  awk -v key="$1" '
    $0 ~ "^" key ":" { hit = 1; next }
    hit && /^[A-Za-z_]/ { exit }
    hit { print }
  ' "$DEFAULTS"
}

for key in public_allow_ports_base internal_allow_ports_base; do
  block="$(allow_list_block "$key")"
  if grep -qE '\b(6061|6062)\b' <<<"$block"; then
    bad "$DEFAULTS: $key lists galexie's admin port (6061/6062) — this would expose it beyond loopback"
  else
    ok "$DEFAULTS: $key does not list galexie's admin port"
  fi
done

echo "ansible-galexie-admin-firewall-test: $pass passed, $fail failed"
[ "$fail" -eq 0 ]
