#!/usr/bin/env bash
# check-main-ci-health-test.sh — fixture tests for the shared red-streak
# decision core (scripts/ci/check-main-ci-health.sh).
#
# The script is generic over CI_WORKFLOW_FILE: ci-health.yml points it
# at `ci.yml` (every push), reconciliation-health.yml points it at
# `ansible-drift.yml` (weekly) with looser thresholds
# (reconciliation-and-monitoring-liveness, audit-2026-07-23). Both
# consumers share this one decision core, so a regression here breaks
# BOTH watchdogs silently — the script had no test coverage before this
# file. Pins the fail-open/fail-red boundary with CI_HEALTH_FIXTURE
# (no network, no gh token).
#
# Run: bash scripts/ci/check-main-ci-health-test.sh
set -uo pipefail

cd "$(dirname "$0")/../.." || exit 1
CHECK="$PWD/scripts/ci/check-main-ci-health.sh"

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

pass=0
fail=0

now_iso() { date -u -v-"${1}"H +%Y-%m-%dT%H:%M:%SZ 2>/dev/null || date -u -d "-${1} hours" +%Y-%m-%dT%H:%M:%SZ; }

# run <fixture-json> [FAIL_RUNS] [FAIL_HOURS]
run() {
  printf '%s' "$1" > "$TMP/fixture.json"
  OUT="$(CI_HEALTH_FIXTURE="$TMP/fixture.json" FAIL_RUNS="${2:-3}" FAIL_HOURS="${3:-6}" bash "$CHECK" 2>&1)"
  RC=$?
}

expect() {
  local name="$1" want_rc="$2" want_sub="${3:-}"
  if [ "$RC" -ne "$want_rc" ]; then
    echo "FAIL: $name — exit $RC, want $want_rc" >&2
    printf '%s\n' "$OUT" | sed 's/^/    /' >&2
    fail=$((fail + 1))
    return
  fi
  if [ -n "$want_sub" ] && ! printf '%s' "$OUT" | grep -q -- "$want_sub"; then
    echo "FAIL: $name — output missing '$want_sub'" >&2
    printf '%s\n' "$OUT" | sed 's/^/    /' >&2
    fail=$((fail + 1))
    return
  fi
  echo "ok: $name"
  pass=$((pass + 1))
}

# ── Healthy shapes ──────────────────────────────────────────────────

run '{"workflow_runs":[{"conclusion":"success","created_at":"'"$(now_iso 1)"'","html_url":"u","head_sha":"aaaaaaa"}]}'
expect 'latest run green → healthy' 0 'HEALTHY'

run '{"workflow_runs":[]}'
expect 'no runs at all → nothing to assess (not faulted)' 0 'nothing to assess'

run '{"workflow_runs":[{"conclusion":"cancelled","created_at":"'"$(now_iso 1)"'","html_url":"u","head_sha":"aaaaaaa"}]}'
expect 'only a cancelled run (no health signal) → nothing to assess' 0 'nothing to assess'

# One red run, well under both thresholds.
run '{"workflow_runs":[{"conclusion":"failure","created_at":"'"$(now_iso 1)"'","html_url":"u","head_sha":"aaaaaaa"},{"conclusion":"success","created_at":"'"$(now_iso 2)"'","html_url":"u","head_sha":"bbbbbbb"}]}' 3 6
expect 'one recent red run, streak+age under threshold → not faulting yet' 0 'not faulting yet'

# ── RED shapes (the exact class ansible-drift.yml is currently in:
#    every scheduled run fails before producing a verdict) ──────────

# Two consecutive red runs meets FAIL_RUNS=2 (reconciliation-health's
# threshold) even though the streak is only ~24h old (well under
# FAIL_HOURS=216) — the RUN-COUNT threshold alone must trip it.
run '{"workflow_runs":[
  {"conclusion":"failure","created_at":"'"$(now_iso 1)"'","html_url":"u1","head_sha":"aaaaaaa"},
  {"conclusion":"failure","created_at":"'"$(now_iso 25)"'","html_url":"u2","head_sha":"bbbbbbb"}
]}' 2 216
expect 'two consecutive red runs meets FAIL_RUNS=2 → RED' 1 'RED —'

# A single red run, but old enough to clear FAIL_HOURS on its own —
# the AGE threshold alone must trip it even with a short streak.
run '{"workflow_runs":[{"conclusion":"failure","created_at":"'"$(now_iso 300)"'","html_url":"u","head_sha":"aaaaaaa"}]}' 5 216
expect 'one old-enough red run meets FAIL_HOURS=216 → RED' 1 'RED —'

# Mirrors the real ansible-drift.yml shape observed 2026-07-25: two
# scheduled runs eight days apart, BOTH failure, no green run anywhere
# in the fetched window — reconciliation-health's actual thresholds
# (FAIL_RUNS=2 FAIL_HOURS=216) must classify this RED.
run '{"workflow_runs":[
  {"conclusion":"failure","created_at":"'"$(now_iso 120)"'","html_url":"https://x/29732030490","head_sha":"39a4e13"},
  {"conclusion":"failure","created_at":"'"$(now_iso 312)"'","html_url":"https://x/29615596967","head_sha":"39a4e13"}
]}' 2 216
expect 'ansible-drift-shaped chronic-red fixture → RED' 1 'RED —'

echo
echo "check-main-ci-health-test: ${pass} passed, ${fail} failed"
[ "$fail" -eq 0 ] || exit 1
