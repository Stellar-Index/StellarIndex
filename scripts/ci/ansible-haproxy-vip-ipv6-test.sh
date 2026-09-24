#!/usr/bin/env bash
# ansible-haproxy-vip-ipv6-test.sh — pins SL23: 07-monitoring.yml's
# keepalived-textfile-scraper detected VIP ownership with
# `ip -4 a show {{ keepalived_iface }} | grep -q {{ haproxy_vip }}`,
# which excludes every IPv6 address on the interface. An IPv6
# haproxy_vip would never be found, so stellarindex_haproxy_vip_owner
# would report 0 on the actual MASTER, forever.
#
# Structural only (grep over the real task file) — no hosts, no ansible run.
set -uo pipefail

cd "$(dirname "$0")/../.." || exit 1
TASKS="${TASKS:-configs/ansible/roles/haproxy/tasks/07-monitoring.yml}"

pass=0; fail=0
ok()  { pass=$((pass + 1)); echo "  ok   — $1"; }
bad() { fail=$((fail + 1)); echo "  FAIL — $1"; }

if [ ! -f "$TASKS" ]; then
  echo "ansible-haproxy-vip-ipv6-test: FAIL — $TASKS not found" >&2
  exit 1
fi

body="$(grep -v '^\s*#' "$TASKS")"

if grep -qE 'ip -4 a show' <<<"$body"; then
  bad "VIP-ownership probe still scoped to -4 — excludes every IPv6 haproxy_vip"
else
  ok "VIP-ownership probe is not scoped to -4"
fi

if grep -qE '^\s*ip a show \{\{ keepalived_iface \}\}' <<<"$body"; then
  ok "VIP-ownership probe lists both address families (ip a show, no -4/-6)"
else
  bad "VIP-ownership probe does not list both address families"
fi

echo "ansible-haproxy-vip-ipv6-test: $pass passed, $fail failed"
[ "$fail" -eq 0 ]
