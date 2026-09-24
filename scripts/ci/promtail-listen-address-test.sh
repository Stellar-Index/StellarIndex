#!/usr/bin/env bash
# promtail-listen-address-test.sh — pins T573/T594: the archival-node
# role's rendered promtail config (and its r1 hand-installed twin)
# set `http_listen_port: 9080` / `grpc_listen_port: 0` with no
# `http_listen_address` / `grpc_listen_address`, so dskit/weaveworks
# server semantics bind both the HTTP and (ephemeral) gRPC listener to
# every interface, not just loopback. The sibling loki role's promtail
# template already sets http_listen_address: 127.0.0.1 — this pins the
# archival-node template and configs/loki/promtail.r1.yml to the same
# loopback-only contract for both listeners.
#
# Structural only (grep over the real files) — no hosts, no ansible run.
set -uo pipefail

cd "$(dirname "$0")/../.." || exit 1

pass=0; fail=0
ok()  { pass=$((pass + 1)); echo "  ok   — $1"; }
bad() { fail=$((fail + 1)); echo "  FAIL — $1"; }

check_loopback_server() {  # <label> <file>
  local label="$1" file="$2"
  if [ ! -f "$file" ]; then bad "$label: $file not found"; return; fi
  local body; body="$(grep -v '^\s*#' "$file")"

  if grep -qE '^\s*http_listen_address:\s*127\.0\.0\.1\s*$' <<<"$body"; then
    ok "$label: http_listen_address pinned to 127.0.0.1"
  else
    bad "$label: no http_listen_address: 127.0.0.1 under server: — the HTTP listener binds all interfaces"
  fi

  if grep -qE '^\s*grpc_listen_address:\s*127\.0\.0\.1\s*$' <<<"$body"; then
    ok "$label: grpc_listen_address pinned to 127.0.0.1"
  else
    bad "$label: no grpc_listen_address: 127.0.0.1 under server: — grpc_listen_port: 0 is ephemeral-all-interfaces, not disabled"
  fi
}

check_loopback_server "archival-node promtail template" \
  "configs/ansible/roles/archival-node/templates/promtail-config.yml.j2"
check_loopback_server "r1 hand-installed promtail config" \
  "configs/loki/promtail.r1.yml"

echo "promtail-listen-address-test: $pass passed, $fail failed"
[ "$fail" -eq 0 ]
