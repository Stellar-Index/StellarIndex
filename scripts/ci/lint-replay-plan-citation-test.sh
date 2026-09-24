#!/usr/bin/env bash
#
# lint-replay-plan-citation-test.sh — regression guard for RSWP-048.
#
# lint-replay-plan.sh's has_replay_plan() comment cited "PR #38" as the PR
# that documents the SIGPIPE-under-pipefail trap in lint-baseline-growth.sh.
# #38 is "Remediation wave 1 (money): audit-2026-07-23 confirmed findings"
# (merged) — an unrelated money-remediation PR that never touched
# lint-baseline-growth.sh. The commit that actually diagnosed and fixed the
# trap (f0e8dd5cf, "make the baseline-growth tripwire's verdict independent
# of range size") shipped as PR #39; #38 only appears in it as the PR whose
# CI run first exposed the bug. A reader following "PR #38" from
# lint-replay-plan.sh lands on the wrong PR and finds nothing about the
# SIGPIPE trap.
#
# Run: bash scripts/ci/lint-replay-plan-citation-test.sh
set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
TARGET="$ROOT/scripts/ci/lint-replay-plan.sh"

fail=0

if [ ! -f "$TARGET" ]; then
  echo "lint-replay-plan-citation-test: FAIL — $TARGET not found" >&2
  exit 1
fi

# The stale, mis-pointed citation must be gone.
if grep -qF -- 'lint-baseline-growth.sh (PR #38)' "$TARGET"; then
  echo "lint-replay-plan-citation-test: FAIL — stale citation 'lint-baseline-growth.sh (PR #38)' still present (RSWP-048: PR #38 is an unrelated money-remediation PR, not the PR that documents the SIGPIPE trap)" >&2
  fail=1
fi

# The correct citation — PR #39, which actually diagnosed and fixed the
# SIGPIPE-under-pipefail regression — must be present instead.
if ! grep -qF -- 'lint-baseline-growth.sh (PR #39)' "$TARGET"; then
  echo "lint-replay-plan-citation-test: FAIL — expected citation 'lint-baseline-growth.sh (PR #39)' missing from $TARGET" >&2
  fail=1
fi

if [ "$fail" -ne 0 ]; then
  exit 1
fi

echo "lint-replay-plan-citation-test: OK — SIGPIPE trap citation points at PR #39, not the unrelated #38"
