#!/usr/bin/env bash
# check-deploy-relax.sh — decide whether a deploy-approval-gate
# relaxation is currently in force (K079, ops-deploy).
#
# DEPLOY_APPROVAL_RELAXED=true lets the gated workflows skip the
# required-reviewers assertion (never the main-only branch policy)
# while r1 carries no production traffic. Pre-fix, that relaxation had
# no expiry: the repo variable could be flipped on once and forgotten
# forever, and nothing in the repo would notice or alarm. This script
# is the single place they ask "is the relaxation still valid?" (via
# assert-deploy-gate.sh), paired with a companion repo variable,
# DEPLOY_APPROVAL_RELAXED_UNTIL (an ISO-8601 date, e.g. 2026-10-01):
# past that date the relaxation is treated as expired and the caller
# must fall through to the real required-reviewers check. Fail CLOSED
# on every ambiguous case (missing expiry, unparseable expiry, expiry
# in the past).
#
# Usage:
#   APPROVAL_RELAXED=... APPROVAL_RELAXED_UNTIL=... \
#     bash scripts/ci/check-deploy-relax.sh <label>
#
# Exit 0 = relaxation active and unexpired; caller may skip the gate.
# Exit 1 = relaxation requested but expired/malformed; fail closed,
#          error already printed to stderr.
# Exit 2 = relaxation not requested; caller must run the real check.
set -uo pipefail

LABEL="${1:-deploy}"

if [ "${APPROVAL_RELAXED:-}" != "true" ]; then
  exit 2
fi

UNTIL="${APPROVAL_RELAXED_UNTIL:-}"
if [ -z "$UNTIL" ]; then
  echo "::error::deploy-approval-gate relaxation for '${LABEL}' has no DEPLOY_APPROVAL_RELAXED_UNTIL expiry set. Refusing an open-ended relaxation; set the repo variable to an ISO-8601 date (e.g. 2026-10-01) or delete DEPLOY_APPROVAL_RELAXED to re-arm." >&2
  exit 1
fi

# GNU date (`-d`) on Actions runners; BSD date (`-j -f`) for local
# macOS development/testing of this script.
UNTIL_EPOCH="$(date -u -d "$UNTIL" +%s 2>/dev/null)"
if [ -z "$UNTIL_EPOCH" ]; then
  UNTIL_EPOCH="$(date -u -j -f '%Y-%m-%d' "$UNTIL" +%s 2>/dev/null)"
fi
if [ -z "$UNTIL_EPOCH" ]; then
  echo "::error::deploy-approval-gate relaxation for '${LABEL}' has an unparseable DEPLOY_APPROVAL_RELAXED_UNTIL='${UNTIL}' (expected YYYY-MM-DD). Refusing on fail-closed default." >&2
  exit 1
fi

NOW_EPOCH="$(date -u +%s)"
if [ "$NOW_EPOCH" -gt "$UNTIL_EPOCH" ]; then
  echo "::error::deploy-approval-gate relaxation for '${LABEL}' EXPIRED on ${UNTIL} (DEPLOY_APPROVAL_RELAXED_UNTIL). Re-arm the gate: delete DEPLOY_APPROVAL_RELAXED, or bump DEPLOY_APPROVAL_RELAXED_UNTIL after reviewing the risk." >&2
  exit 1
fi

echo "::notice::deploy-approval-gate intentionally relaxed for '${LABEL}' until ${UNTIL} (DEPLOY_APPROVAL_RELAXED=true). RE-ARM BEFORE PRODUCTION LAUNCH: delete the DEPLOY_APPROVAL_RELAXED repo variable and configure the '${LABEL}' environment Required-reviewers rule."
exit 0
