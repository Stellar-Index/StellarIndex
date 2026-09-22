#!/usr/bin/env bash
# ansible-exporter-listen-address-test.sh — pins T575: the Debian-packaged
# redis_exporter / postgres_exporter systemd units in the archival-node
# role read `EnvironmentFile=/etc/default/<pkg>` and exec
# `<binary> $ARGS` (same pattern this role already uses for node_exporter
# and local-prometheus). WEB_LISTEN_ADDRESS / PG_EXPORTER_WEB_LISTEN_ADDRESS
# are not real inputs to that ExecStart line, so setting only those vars
# (under a "defence-in-depth" comment) left both exporters listening on
# every interface at their default port despite the env-file claiming
# loopback-only. The fix passes the address as a `--web.listen-address`
# CLI flag via ARGS.
#
# Structural only (grep over the real task file) — no hosts, no ansible run.
set -uo pipefail

cd "$(dirname "$0")/../.." || exit 1
TASKS="${TASKS:-configs/ansible/roles/archival-node/tasks/16-prometheus-exporters.yml}"

pass=0; fail=0
ok()  { pass=$((pass + 1)); echo "  ok   — $1"; }
bad() { fail=$((fail + 1)); echo "  FAIL — $1"; }

if [ ! -f "$TASKS" ]; then
  echo "ansible-exporter-listen-address-test: FAIL — $TASKS not found" >&2
  exit 1
fi

# task_block <file> <pattern> — the task from its `- name:` line up to
# (not including) the next top-level `- name:`.
task_block() {
  awk -v pat="$2" '
    /^- name:/ { if (hit) { print buf; exit } buf = ""; hit = 0 }
    { buf = buf $0 "\n"; t = $0; sub(/^[ \t]+/, "", t); sub(/[ \t]+$/, "", t); if (t == pat) hit = 1 }
    END { if (hit) print buf }' "$1"
}
uncommented() { grep -v '^\s*#' <<<"$1"; }

check_exporter() {  # <unit-name> <task-name> <loopback-addr>
  local unit="$1" name="$2" addr="$3"
  local blk; blk="$(task_block "$TASKS" "$name")"
  if [ -z "$blk" ]; then bad "$unit: task '$name' not found"; return; fi
  local body; body="$(uncommented "$blk")"

  if grep -qE "^[[:space:]]*ARGS=\"[^\"]*--web\.listen-address=${addr}[^\"]*\"" <<<"$body"; then
    ok "$unit: ARGS pins --web.listen-address=$addr"
  else
    bad "$unit: no ARGS='--web.listen-address=$addr ...' line — the unit's ExecStart is '\$ARGS'-driven, so a bare env var here is inert and the exporter binds all interfaces"
  fi
}

check_exporter "prometheus-redis-exporter" "dest: /etc/default/prometheus-redis-exporter" "127.0.0.1:9121"
check_exporter "prometheus-postgres-exporter" "dest: /etc/default/prometheus-postgres-exporter" "127.0.0.1:9187"

echo "ansible-exporter-listen-address-test: $pass passed, $fail failed"
[ "$fail" -eq 0 ]
