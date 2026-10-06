#!/usr/bin/env bash
# ansible-galexie-backfill-status-test.sh — galexie-backfill-status.sh must
# survive an empty/missing backfill log under `set -u`: the rate section
# reads now_epoch, which used to be assigned only when the log had a start
# timestamp. Runs a path-stubbed copy against temp files; no hosts.
set -uo pipefail

cd "$(dirname "$0")/../.." || exit 1
SCRIPT="${SCRIPT:-configs/ansible/roles/archival-node/files/galexie-backfill-status.sh}"
[ -f "$SCRIPT" ] || { echo "ansible-galexie-backfill-status-test: FAIL — $SCRIPT not found" >&2; exit 1; }

tmp="$(mktemp -d)"; trap 'rm -rf "$tmp"' EXIT
sed "s#/var/log/#$tmp/#g;s#/var/lib/#$tmp/#g" "$SCRIPT" > "$tmp/s.sh"

pass=0; fail=0
ok()  { pass=$((pass + 1)); echo "  ok   — $1"; }
bad() { fail=$((fail + 1)); echo "  FAIL — $1"; }

: > "$tmp/galexie-backfill.log"
out="$(bash "$tmp/s.sh" 2>&1)"; rc=$?
if [ "$rc" -eq 0 ] && ! grep -q 'unbound variable' <<<"$out"; then
  ok "empty log: exits 0, no unbound variable"
else
  bad "empty log: rc=$rc — $(grep -m1 'unbound variable' <<<"$out")"
fi

rm -f "$tmp/galexie-backfill.log"
out="$(bash "$tmp/s.sh" 2>&1)"; rc=$?
if [ "$rc" -eq 0 ] && ! grep -q 'unbound variable' <<<"$out"; then
  ok "missing log: exits 0, no unbound variable"
else
  bad "missing log: rc=$rc — $(grep -m1 'unbound variable' <<<"$out")"
fi

echo "ansible-galexie-backfill-status-test: $pass ok, $fail failed"
[ "$fail" -eq 0 ]
