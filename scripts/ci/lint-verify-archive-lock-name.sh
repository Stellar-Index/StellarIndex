#!/usr/bin/env bash
# lint-verify-archive-lock-name.sh — the verify-archive-tier-a.service.j2
# timer and the runbooks' "run it manually" instructions must pass the
# SAME job name to run-heavy-job.sh. run-heavy-job.sh's flock is keyed on
# that first argument (C4-14 / INF-11): a manual re-run under a different
# name doesn't share the scheduled unit's singleton lock, so an operator
# following the runbook mid-incident can run concurrently with the timer
# instead of being skipped with exit 75 (F153).
#
# Run: bash scripts/ci/lint-verify-archive-lock-name.sh
set -uo pipefail
cd "$(dirname "$0")/../.." || exit 1

UNIT_J2="configs/ansible/roles/archival-node/templates/systemd/verify-archive-tier-a.service.j2"
RUNBOOKS=(
  docs/operations/runbooks/verify-archive-unit-failed.md
  docs/operations/runbooks/verify-archive-run-stale.md
)

[ -f "$UNIT_J2" ] || { echo "lint-verify-archive-lock-name: $UNIT_J2 missing"; exit 2; }

canonical=$(grep -oE '^ExecStart=/usr/local/sbin/run-heavy-job\.sh [a-zA-Z0-9_-]+' "$UNIT_J2" | awk '{print $2}')
if [ -z "$canonical" ]; then
  echo "lint-verify-archive-lock-name: could not find the ExecStart job name in $UNIT_J2" >&2
  exit 2
fi

fail=0
for rb in "${RUNBOOKS[@]}"; do
  [ -f "$rb" ] || { echo "lint-verify-archive-lock-name: $rb missing"; exit 2; }
  # Only lines that wrap a verify-archive invocation (fenced-code
  # run-heavy-job.sh call immediately preceding a verify-archive command
  # a couple of lines later); grep -A2 catches the wrapped command.
  while IFS= read -r name; do
    [ -n "$name" ] || continue
    if [ "$name" != "$canonical" ]; then
      echo "  MISMATCH in $rb: run-heavy-job.sh uses '$name', $UNIT_J2 uses '$canonical'"
      fail=$((fail + 1))
    fi
  done < <(grep -A2 '/usr/local/sbin/run-heavy-job\.sh' "$rb" | grep -B2 'verify-archive' | grep -oE '^\s*/usr/local/sbin/run-heavy-job\.sh [a-zA-Z0-9_-]+' | awk '{print $2}')
done

if [ "$fail" -gt 0 ]; then
  echo "lint-verify-archive-lock-name: $fail mismatched job name(s)"
  exit 1
fi
echo "lint-verify-archive-lock-name: OK — runbooks and the systemd template agree on the verify-archive lock name ('$canonical')."
