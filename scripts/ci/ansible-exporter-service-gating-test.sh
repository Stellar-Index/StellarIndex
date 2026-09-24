#!/usr/bin/env bash
# ansible-exporter-service-gating-test.sh — pins T485: tasks/main.yml
# imports 16-prometheus-exporters.yml gated only on `run_observability`
# (no run_postgres / pgbackrest_backup_enabled). Inside that file, the
# postgres_exporter tasks (including a `psql -d stellarindex` GRANT run
# as become_user postgres with failed_when rc != 0) and the
# pgbackrest_exporter install/start tasks ran unconditionally on ANY
# run_observability host, including one with no Postgres and one with
# backups disabled — the exact asymmetry defaults/main.yml's own
# scrape-list Jinja avoids by gating those two jobs on run_postgres and
# pgbackrest_backup_enabled respectively.
#
# Structural only (grep over the real task file) — no hosts, no ansible run.
set -uo pipefail

cd "$(dirname "$0")/../.." || exit 1
TASKS="${TASKS:-configs/ansible/roles/archival-node/tasks/16-prometheus-exporters.yml}"

pass=0; fail=0
ok()  { pass=$((pass + 1)); echo "  ok   — $1"; }
bad() { fail=$((fail + 1)); echo "  FAIL — $1"; }

if [ ! -f "$TASKS" ]; then
  echo "ansible-exporter-service-gating-test: FAIL — $TASKS not found" >&2
  exit 1
fi

# block_body <file> <pattern> — the top-level `- name: <pattern>` entry's
# body (its `block:` contents), from its line to the next top-level
# `- name:` at the same (zero) indent.
block_body() {
  awk -v pat="$2" '
    /^- name:/ {
      if (hit) { print buf; exit }
      buf = ""; hit = 0
      t = $0; sub(/^- name: /, "", t)
      if (t == pat) hit = 1
      next
    }
    { buf = buf $0 "\n" }
    END { if (hit) print buf }' "$1"
}

pg_body="$(block_body "$TASKS" "Group B — postgres_exporter (requires Postgres)")"
if [ -z "$pg_body" ]; then
  bad "Group B (postgres_exporter) task/block not found"
elif grep -qE '^\s*when:\s*run_postgres\s*\|\s*bool\s*$' <<<"$pg_body"; then
  ok "Group B (postgres_exporter, incl. the psql GRANT) is gated on run_postgres"
else
  bad "Group B has no 'when: run_postgres | bool' guard — the psql GRANT runs on a host with no Postgres"
fi

pgbr_body="$(block_body "$TASKS" "Group C — pgbackrest_exporter (requires pgbackrest backups)")"
if [ -z "$pgbr_body" ]; then
  bad "Group C (pgbackrest_exporter) task/block not found"
elif grep -qE '^\s*when:\s*pgbackrest_backup_enabled\s*\|\s*default\(true\)\s*\|\s*bool\s*$' <<<"$pgbr_body"; then
  ok "Group C (pgbackrest_exporter) is gated on pgbackrest_backup_enabled"
else
  bad "Group C has no 'when: pgbackrest_backup_enabled | default(true) | bool' guard — installs/starts even with backups disabled"
fi

echo "ansible-exporter-service-gating-test: $pass passed, $fail failed"
[ "$fail" -eq 0 ]
