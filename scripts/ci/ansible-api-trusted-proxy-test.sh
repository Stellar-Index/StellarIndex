#!/usr/bin/env bash
# ansible-api-trusted-proxy-test.sh — an inventory that binds the API off
# loopback is fronted by a proxy on another address, and must name that
# address as trusted. With the role default (loopback) the API ignores
# X-Forwarded-For from it, every client resolves to the proxy's address, and
# all of them share one rate-limit bucket.
#
# Structural only (grep over the real files): no hosts, no ansible run.
set -uo pipefail

cd "$(dirname "$0")/../.." || exit 1
TEMPLATE="configs/ansible/roles/archival-node/templates/stellarindex.toml.j2"
DEFAULTS="configs/ansible/roles/archival-node/defaults/main.yml"
VAR=stellarindex_api_trusted_proxy_cidrs

pass=0; fail=0
ok()  { pass=$((pass + 1)); echo "  ok   — $1"; }
bad() { fail=$((fail + 1)); echo "  FAIL — $1"; }

if grep -qE "^trusted_proxy_cidrs[[:space:]]*=[[:space:]]*\{\{[[:space:]]*${VAR}[[:space:]]*\|[[:space:]]*to_json" "$TEMPLATE"; then
  ok "$TEMPLATE: trusted_proxy_cidrs derives from $VAR"
else
  bad "$TEMPLATE: trusted_proxy_cidrs is not templated from $VAR — an inventory cannot name its proxy"
fi

if grep -qE "^${VAR}:[[:space:]]*\[\"127\.0\.0\.1/32\"\]" "$DEFAULTS"; then
  ok "$DEFAULTS: $VAR defaults to loopback"
else
  bad "$DEFAULTS: $VAR is missing or does not default to loopback only"
fi

seen=0
for inv in configs/ansible/inventory/*.example.yml; do
  [ -e "$inv" ] || continue
  grep -qE '^[[:space:]]*stellarindex_api_listen_addr:[[:space:]]*"?0\.0\.0\.0:' "$inv" || continue
  seen=$((seen + 1))
  if grep -qE "^[[:space:]]*${VAR}:[[:space:]]*\[\"[0-9]" "$inv"; then
    ok "$inv: binds the API off loopback and names its proxy"
  else
    bad "$inv: binds the API on 0.0.0.0 but does not set $VAR"
  fi
done
if [ "$seen" -gt 0 ]; then
  ok "$seen example inventor(ies) bind the API off loopback"
else
  bad "no example inventory binds the API off loopback — the check above tested nothing"
fi

echo "ansible-api-trusted-proxy-test: $pass passed, $fail failed"
[ "$fail" -eq 0 ]
