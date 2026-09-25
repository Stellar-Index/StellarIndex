#!/usr/bin/env bash
# ansible-minio-buckets-var-test.sh — pins Q243: the archival-node role
# declares `minio_buckets` in defaults/main.yml and several inventories
# (r2/r3/futurenet/testnet) override it to a narrower list (galexie-live
# only hosts drop galexie-archive), but the "Ensure required MinIO buckets
# exist" task in 09-minio.yml hard-coded its own
# [galexie-live, galexie-archive, backups] loop instead of reading the
# variable — every inventory override was dead and every host created all
# three buckets regardless of role. The fix loops `{{ minio_buckets }}`.
#
# Structural only (grep over the real task file) — no hosts, no ansible run.
set -uo pipefail

cd "$(dirname "$0")/../.." || exit 1
TASKS="${TASKS:-configs/ansible/roles/archival-node/tasks/09-minio.yml}"

pass=0; fail=0
ok()  { pass=$((pass + 1)); echo "  ok   — $1"; }
bad() { fail=$((fail + 1)); echo "  FAIL — $1"; }

if [ ! -f "$TASKS" ]; then
  echo "ansible-minio-buckets-var-test: FAIL — $TASKS not found" >&2
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

blk="$(task_block "$TASKS" "- name: Ensure required MinIO buckets exist")"
if [ -z "$blk" ]; then
  bad "task 'Ensure required MinIO buckets exist' not found"
else
  body="$(uncommented "$blk")"
  if grep -qE '^[[:space:]]*loop:[[:space:]]*"\{\{[[:space:]]*minio_buckets[[:space:]]*\}\}"' <<<"$body"; then
    ok "loop reads the minio_buckets variable"
  else
    bad "loop does not read minio_buckets — inventory overrides (r2/r3/futurenet/testnet) are dead"
  fi
  if grep -qE '^\s*-\s*galexie-live\s*$' <<<"$body"; then
    bad "loop still hard-codes a literal bucket list alongside/instead of the variable"
  else
    ok "no hard-coded literal bucket list remains in the loop"
  fi
fi

echo "ansible-minio-buckets-var-test: $pass passed, $fail failed"
[ "$fail" -eq 0 ]
