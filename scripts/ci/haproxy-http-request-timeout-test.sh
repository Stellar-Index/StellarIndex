#!/usr/bin/env bash
# haproxy-http-request-timeout-test.sh — pins SL01: the haproxy role's
# `defaults` block set `timeout connect`, `timeout client` and
# `timeout server` but no `timeout http-request`. Without it, a client
# that opens a connection and trickles request headers (slowloris) holds
# a server slot for the full 60s client timeout instead of being
# dropped quickly, exhausting maxconn under a slow-header attack.
#
# Structural only (grep over the rendered-template source) — no hosts,
# no ansible run.
set -uo pipefail

cd "$(dirname "$0")/../.." || exit 1
TEMPLATE="${TEMPLATE:-configs/ansible/roles/haproxy/templates/haproxy.cfg.j2}"

pass=0; fail=0
ok()  { pass=$((pass + 1)); echo "  ok   — $1"; }
bad() { fail=$((fail + 1)); echo "  FAIL — $1"; }

if [ ! -f "$TEMPLATE" ]; then
  echo "haproxy-http-request-timeout-test: FAIL — $TEMPLATE not found" >&2
  exit 1
fi

# defaults_block — the `defaults` section, from its header line up to
# (not including) the next top-level (unindented) section.
defaults_block() {
  awk '
    /^defaults/ { hit = 1; print; next }
    hit && /^[a-zA-Z]/ { exit }
    hit { print }
  ' "$TEMPLATE"
}

blk="$(defaults_block)"
if [ -z "$blk" ]; then
  bad "no 'defaults' block found in $TEMPLATE"
elif grep -qE '^\s*timeout\s+http-request\s+[0-9]+(s|ms)?\s*$' <<<"$blk"; then
  ok "defaults block sets 'timeout http-request'"
else
  bad "defaults block has no 'timeout http-request' — a slow-header client holds a server slot for the full 'timeout client' (60s) instead of being dropped quickly"
fi

echo "haproxy-http-request-timeout-test: $pass passed, $fail failed"
[ "$fail" -eq 0 ]
