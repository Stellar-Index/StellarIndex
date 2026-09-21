#!/usr/bin/env bash
# check-verify-parity-ci-wiring-evidence.sh — evidence for RLT-377 / T488:
# the verify.sh<->CI parity gate (check-verify-parity.sh) is never itself
# invoked by any CI workflow, only by scripts/dev/verify.sh (a script a
# contributor runs locally, not CI). Closing this needs a
# .github/workflows/*.yml change — outside u-T020's file fence
# (docs/reference/config, scripts/ci/*, scripts/dev/verify.sh,
# web/explorer/src/api/types.ts) — see the u-T020 fixer report
# (NEEDS-COORDINATION on RLT-377 / T488).
#
# Deliberately named without a `-test.sh` suffix: that suffix is
# lint-changed.sh's auto-discovery convention for a script it RUNS and
# requires to pass, and this one is expected to FAIL until the
# coordination fix lands — it is a reproduction, not a gate. NOT wired
# into verify.sh or lint-changed.sh. Run manually:
#   bash scripts/ci/check-verify-parity-ci-wiring-evidence.sh
# Passes once some .github/workflows/*.yml invokes check-verify-parity.sh
# directly, or runs scripts/dev/verify.sh / `make verify` as a real step
# (not merely mentioned in a comment).
set -euo pipefail
cd "$(dirname "$0")/../.."

if grep -lE '(run:.*(\./|bash +)scripts/(ci/check-verify-parity\.sh|dev/verify\.sh))|(run: *make verify\b)' \
     .github/workflows/*.yml >/dev/null 2>&1; then
  echo "check-verify-parity-ci-wiring: OK — a CI workflow invokes check-verify-parity.sh (directly, or via verify.sh / make verify)."
  exit 0
fi

echo "check-verify-parity-ci-wiring: FAIL — no .github/workflows/*.yml runs check-verify-parity.sh, scripts/dev/verify.sh, or 'make verify' as a step." >&2
echo "  RLT-377 / T488: the verify.sh<->CI parity gate never runs in CI, only when a contributor runs verify.sh locally." >&2
exit 1
