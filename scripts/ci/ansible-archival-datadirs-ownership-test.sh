#!/usr/bin/env bash
# ansible-archival-datadirs-ownership-test.sh — pins Q247: on a no-ZFS host
# (zfs_data_pool_type unset — r2/r3 on EBS/object storage, or a libvirt VM),
# tasks/main.yml's "Create data dirs as plain directories" task creates
# every zfs_datasets mountpoint EXCEPT postgres/clickhouse as plain
# directories with only `mode` set, never `owner`/`group`. The restore-drill
# dataset (defaults/main.yml) declares dir_owner/dir_group: postgres because
# pgbackrest (running as postgres) creates its scratch pgdata dir inside the
# mountpoint (BDR-04) — but nothing else in the role chowns /srv/restore-drill
# on a no-ZFS host, so pgbackrest hit a root-owned, postgres-unwritable dir.
# 03-zfs.yml's own "Ensure dataset mount points exist with correct perms"
# task already sets owner/group from item.dir_owner/dir_group with
# default(omit) — the no-ZFS task must mirror it.
#
# Structural only (grep over the real task file) — no hosts, no ansible run.
set -uo pipefail

cd "$(dirname "$0")/../.." || exit 1
TASKS="${TASKS:-configs/ansible/roles/archival-node/tasks/main.yml}"

pass=0; fail=0
ok()  { pass=$((pass + 1)); echo "  ok   — $1"; }
bad() { fail=$((fail + 1)); echo "  FAIL — $1"; }

if [ ! -f "$TASKS" ]; then
  echo "ansible-archival-datadirs-ownership-test: FAIL — $TASKS not found" >&2
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

blk="$(task_block "$TASKS" '- name: "Create data dirs as plain directories (no-ZFS hosts)"')"
if [ -z "$blk" ]; then
  bad "no-ZFS data-dirs task not found in $TASKS"
else
  body="$(uncommented "$blk")"
  if grep -qE '^\s*owner:\s*"\{\{\s*item\.dir_owner\s*\|\s*default\(omit\)\s*\}\}"' <<<"$body"; then
    ok "no-ZFS data-dirs task sets owner from item.dir_owner (default omit)"
  else
    bad "no-ZFS data-dirs task has no owner: from item.dir_owner — restore-drill's postgres ownership is silently dropped on a no-ZFS host"
  fi
  if grep -qE '^\s*group:\s*"\{\{\s*item\.dir_group\s*\|\s*default\(omit\)\s*\}\}"' <<<"$body"; then
    ok "no-ZFS data-dirs task sets group from item.dir_group (default omit)"
  else
    bad "no-ZFS data-dirs task has no group: from item.dir_group"
  fi
fi

echo "ansible-archival-datadirs-ownership-test: $pass passed, $fail failed"
[ "$fail" -eq 0 ]
