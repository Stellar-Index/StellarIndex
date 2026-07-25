#!/usr/bin/env bash
# check-deploy-protection-test.sh — fixture tests for the deploy
# approval gate assertion (deploy-approval-gate, audit-2026-07-23).
#
# Pins the fail-CLOSED contract: every shape that is not "a
# required_reviewers rule with at least one reviewer" must exit
# non-zero. A gate that mistakes `"protection_rules": []` for a pass is
# exactly the hole this check was written to close.
#
# Run: bash scripts/ci/check-deploy-protection-test.sh
set -uo pipefail

cd "$(dirname "$0")/../.." || exit 1
CHECK="$PWD/scripts/ci/check-deploy-protection.sh"

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

pass=0
fail=0

# run <json-body> <env-name>
run() {
  printf '%s' "$1" > "$TMP/env.json"
  OUT="$(bash "$CHECK" "$TMP/env.json" "$2" 2>&1)"
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

# The shape the r1 environment actually had when the audit ran
# (gh api repos/Stellar-Index/StellarIndex/environments, 2026-07-24).
run '{"name":"r1","protection_rules":[]}' r1
expect 'no protection rules at all → fail closed' 1 'gate MISSING'

# A wait_timer is not an approval — nobody has to look at it.
run '{"name":"r1","protection_rules":[{"id":1,"type":"wait_timer","wait_timer":30}]}' r1
expect 'wait_timer alone → fail closed' 1 'gate MISSING'

# The rule exists but its reviewer list is empty: GitHub treats this as
# no gate, and so must we.
run '{"name":"r1","protection_rules":[{"id":1,"type":"required_reviewers","reviewers":[]}]}' r1
expect 'required_reviewers with zero reviewers → fail closed' 1 'gate MISSING'

run '{"name":"r1","protection_rules":[{"id":1,"type":"required_reviewers","reviewers":[{"type":"User","reviewer":{"login":"op"}}],"prevent_self_review":false}]}' r1
expect 'one reviewer → pass' 0 'approval gate present'

run '{"name":"r1","protection_rules":[{"id":1,"type":"required_reviewers","reviewers":[{"type":"User","reviewer":{"login":"op"}}],"prevent_self_review":false}]}' r1
expect 'self-review allowed is reported, not fatal' 0 'allows self-review'

run '{"name":"r1","protection_rules":[{"id":1,"type":"required_reviewers","reviewers":[{"type":"Team","reviewer":{"slug":"ops"}}],"prevent_self_review":true}]}' r1
expect 'team reviewer + prevent_self_review → pass' 0 'prevent_self_review=true'

# Wrong environment in the response = the caller asked for the wrong
# thing; passing here would verify a gate on some OTHER environment.
run '{"name":"staging","protection_rules":[{"id":1,"type":"required_reviewers","reviewers":[{"type":"User","reviewer":{"login":"op"}}]}]}' r1
expect 'environment name mismatch → fail closed' 1 'expected'

run '{"message":"Not Found","status":"404"}' r1
expect '404 body → fail closed' 1 'NOT VERIFIED'

run 'not json at all' r1
expect 'garbage body → fail closed' 1 'not valid JSON'

run '' r1
expect 'empty body → fail closed' 1 'missing or empty'

echo
echo "check-deploy-protection-test: ${pass} passed, ${fail} failed"
[ "$fail" -eq 0 ] || exit 1
