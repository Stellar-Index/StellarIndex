#!/usr/bin/env bash
# redis-oom-reject-alert-test.sh — regression guard for T611.
#
# archival-node/tasks/15-log-discipline.yml codified `maxmemory` for
# r1's Debian-packaged redis but never `maxmemory-policy`, so a rebuild
# ran on whatever the package's implicit default was (noeviction) with
# no code asserting it. Under noeviction, a full-memory Redis REJECTS
# writes with an OOM error instead of evicting — a failure mode
# stellarindex_redis_evictions_high (an eviction-RATE alert) cannot
# see: it stays at 0 while every write fails. This pins both halves of
# the fix:
#   - 15-log-discipline.yml explicitly pins maxmemory-policy;
#   - both cache.yml rule trees carry an OOM write-rejection alert.
#
# Run: bash scripts/ci/redis-oom-reject-alert-test.sh
set -uo pipefail

cd "$(dirname "$0")/../.." || exit 1

pass=0
fail=0
record() {
  local desc="$1" ok="$2"
  if [ "$ok" = "1" ]; then
    echo "PASS: $desc"
    pass=$((pass + 1))
  else
    echo "FAIL: $desc"
    fail=$((fail + 1))
  fi
}

TASKS="configs/ansible/roles/archival-node/tasks/15-log-discipline.yml"
policy_ok=0
grep -q "maxmemory-policy" "$TASKS" && policy_ok=1
record "$TASKS codifies maxmemory-policy" "$policy_ok"

for f in configs/prometheus/rules.r1/cache.yml deploy/monitoring/rules/cache.yml; do
  alert_ok=0
  if grep -q 'alert: stellarindex_redis_write_rejected_oom' "$f" \
    && grep -q 'redis_errors_total{err="OOM"}' "$f"; then
    alert_ok=1
  fi
  record "$f defines an OOM write-rejection alert" "$alert_ok"
done

echo "redis-oom-reject-alert-test: $pass passed, $fail failed"
[ "$fail" -eq 0 ]
