#!/usr/bin/env bash
# site-crawl-escalation-test.sh — structural regression test for T332.
#
# T332: .github/workflows/site-crawl.yml ran the weekly production crawl
# with no escalation path on failure — a regression just turned the
# Actions tab red for a run nobody watches, indistinguishable from every
# other quiet failure. This asserts the workflow carries the same
# open/update/close tracking-issue shape as its siblings
# (public-dataset-check.yml, dns-perimeter-check.yml): issues: write
# permission, the crawl step's exit code captured into a verdict_rc
# output, an escalation step gated on verdict_rc != '0' that calls the
# GitHub issue API, and a recovery step gated on verdict_rc == '0' that
# closes it again.
#
# Run: bash scripts/ci/site-crawl-escalation-test.sh
set -uo pipefail

cd "$(dirname "$0")/../.." || exit 1
WF=".github/workflows/site-crawl.yml"

pass=0
fail=0

check() {
  local desc="$1" pattern="$2"
  if grep -qE "$pattern" "$WF"; then
    pass=$((pass + 1))
  else
    echo "FAIL: $desc (pattern not found: $pattern)" >&2
    fail=$((fail + 1))
  fi
}

check "issues: write permission granted" '^\s*issues:\s*write\s*$'
check "crawl step captures a verdict_rc output" 'verdict_rc='
check "an escalation step is gated on verdict_rc != .0." "verdict_rc.*!=.*'0'"
check "escalation step opens or comments on a tracking issue" 'gh issue (create|comment)'
check "a recovery step is gated on verdict_rc == .0." "verdict_rc.*==.*'0'"
check "recovery step closes the tracking issue" 'gh issue close'

echo
echo "site-crawl-escalation-test: ${pass} passed, ${fail} failed"
[ "$fail" -eq 0 ]
