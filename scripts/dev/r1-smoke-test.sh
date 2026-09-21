#!/usr/bin/env bash
# r1-smoke-test.sh — regression test for scripts/dev/r1-smoke.sh's
# issue-reference hygiene (RSWP-092).
#
# GitHub issue #1131 is real but unrelated to security.txt (it's the
# fail-open dwell-window ticket) — a dangling-reference sweep flagged
# r1-smoke.sh for citing #1131 as "the PR that ships security.txt",
# which never existed under that number. The /.well-known/security.txt
# route (internal/api/v1/server.go's handleSecurityTxt) is already live
# on this branch, so the fix promotes the check out of the "queued for
# promotion" block instead of leaving a wrong citation behind.
#
# Run: bash scripts/dev/r1-smoke-test.sh
set -uo pipefail

cd "$(dirname "$0")/../.." || exit 1
SMOKE="scripts/dev/r1-smoke.sh"

pass=0
fail=0
ok()  { echo "  ok   $1"; pass=$((pass + 1)); }
bad() { echo "  FAIL $1"; fail=$((fail + 1)); }

if grep -q '1131' "$SMOKE"; then
  bad "no dangling #1131 citation remains (GitHub #1131 is the unrelated dwell-window issue, not security.txt)"
else
  ok "no dangling #1131 citation remains"
fi

# The route already ships (internal/api/v1/server.go registers GET
# /.well-known/security.txt) so the smoke must assert it live, not sit
# behind a commented-out "queued for promotion" line.
if grep -Eq '^check "security\.txt"[[:space:]]+"/\.well-known/security\.txt"' "$SMOKE"; then
  ok "security.txt is checked live (uncommented)"
else
  bad "security.txt is checked live (uncommented)"
fi

if grep -q '/.well-known/security.txt.*queued\|# .*/.well-known/security.txt 200' "$SMOKE"; then
  bad "security.txt is no longer listed as a pending promotion"
else
  ok "security.txt is no longer listed as a pending promotion"
fi

echo "r1-smoke-test: $pass passed, $fail failed"
[ "$fail" -eq 0 ]
