#!/usr/bin/env bash
# audit-remediation-operator-actions-test.sh — pins the CS-097 entry in
# docs/operations/audit-remediation-operator-actions.md to what the live
# repo actually has (RLT-378).
#
# The doc claimed `- [x] ... DONE 2026-07-02 via two repo rulesets`, but
# `gh api repos/Stellar-Index/StellarIndex/branches/main/protection` 404s
# and `gh api .../rulesets` returns `[]` — no protection exists. The
# lint-actions-pinning.sh header made the same false claim about
# allowed_actions/require_sha_pinning being "configured via the GitHub
# admin UI" (live: allowed_actions=all, sha_pinning_required=false).
# Both must say the gap is open, not closed, until an operator applies it.
#
# Run: bash scripts/ci/audit-remediation-operator-actions-test.sh
set -uo pipefail

cd "$(dirname "$0")/../.." || exit 1
DOC="docs/operations/audit-remediation-operator-actions.md"
LINT="scripts/ci/lint-actions-pinning.sh"

pass=0
fail=0
check() { # check <desc> <cond-exit>
  local desc="$1" got="$2"
  if [ "$got" -eq 0 ]; then
    echo "  ok   $desc"
    pass=$((pass + 1))
  else
    echo "  FAIL $desc"
    fail=$((fail + 1))
  fi
}

# The stale "already done" claim must be gone. The backtick in the pattern
# is literal (matching markdown code-span syntax), not command substitution.
# shellcheck disable=SC2016
grep -qE '^- \[x\] \*\*Branch-protect `main`.*DONE 2026-07-02' "$DOC"
check "CS-097 no longer claims DONE via two repo rulesets that don't exist" $((! $?))

# The entry must now be an open checkbox recording the real, re-verified state.
# shellcheck disable=SC2016
grep -qE '^- \[ \] \*\*Branch-protect `main`.*NOT DONE' "$DOC"
check "CS-097 is recorded as an open item" $?

grep -q '404' "$DOC" && grep -q 'rulesets' "$DOC"
check "CS-097 entry cites the live 404/empty-rulesets evidence" $?

# lint-actions-pinning.sh must not assert the repo-settings half is
# configured when it is not.
grep -qE 'F-1216.*is configured via the GitHub admin UI' "$LINT"
check "lint-actions-pinning.sh no longer asserts the unverified 'configured' claim" $((! $?))

grep -q 'allowed_actions: "all"' "$LINT"
check "lint-actions-pinning.sh records the live allowed_actions=all gap" $?

echo "audit-remediation-operator-actions-test: $pass passed, $fail failed"
[ "$fail" -eq 0 ]
