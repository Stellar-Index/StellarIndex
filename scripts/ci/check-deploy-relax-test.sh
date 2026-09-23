#!/usr/bin/env bash
# check-deploy-relax-test.sh — fixture tests for the deploy-approval-gate
# relaxation expiry (K079, ops-deploy).
#
# Pins the fail-CLOSED contract added for K079: a relaxation with no
# expiry, an unparseable expiry, or a past expiry must NOT skip the
# gate. Only a well-formed, still-future expiry may.
#
# Run: bash scripts/ci/check-deploy-relax-test.sh
set -uo pipefail

cd "$(dirname "$0")/../.." || exit 1
CHECK="$PWD/scripts/ci/check-deploy-relax.sh"

pass=0
fail=0

# run <relaxed> <until>
run() {
  OUT="$(APPROVAL_RELAXED="$1" APPROVAL_RELAXED_UNTIL="$2" bash "$CHECK" r1 2>&1)"
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
  if [ -n "$want_sub" ] && ! grep -q -- "$want_sub" <<<"$OUT"; then
    echo "FAIL: $name — output missing '$want_sub'" >&2
    printf '%s\n' "$OUT" | sed 's/^/    /' >&2
    fail=$((fail + 1))
    return
  fi
  echo "ok: $name"
  pass=$((pass + 1))
}

run "" ""
expect 'not relaxed → fall through to real check' 2

# The K079 defect: relaxed with no expiry at all must NOT be treated
# as an indefinite pass.
run "true" ""
expect 'relaxed with no expiry → fail closed' 1 'no DEPLOY_APPROVAL_RELAXED_UNTIL'

run "true" "not-a-date"
expect 'relaxed with unparseable expiry → fail closed' 1 'unparseable'

run "true" "2020-01-01"
expect 'relaxed with expiry in the past → fail closed' 1 'EXPIRED'

FUTURE="$(date -u -v+30d +%Y-%m-%d 2>/dev/null || date -u -d '+30 days' +%Y-%m-%d)"
run "true" "$FUTURE"
expect 'relaxed with a future expiry → active, skip the gate' 0 'intentionally relaxed'

echo "----"
echo "pass=$pass fail=$fail"
[ "$fail" -eq 0 ]
