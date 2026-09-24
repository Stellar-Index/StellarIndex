#!/usr/bin/env bash
# lint-alerts-catalog-test.sh — prove lint-alerts-catalog.py's Runbook-column
# check can go red (RLT-010). It runs the gate against a mutated COPY of the
# catalogue (ALERTS_CATALOG), so the tracked file is never touched.
set -uo pipefail
cd "$(dirname "$0")/../.." || exit 1

GATE="scripts/ci/lint-alerts-catalog.py"
CATALOG="docs/operations/alerts-catalog.md"
PASS=0; FAIL=0

TMP="$(mktemp)"
trap 'rm -f "$TMP"' EXIT

check() { # <name> <expected ok|red> <catalogue path> [<problem substring>]
  local name="$1" want="$2" path="$3" needle="${4:-}" rc out
  out="$(ALERTS_CATALOG="$path" python3 "$GATE" 2>&1)"; rc=$?
  if [ "$want" = ok ] && [ "$rc" -eq 0 ]; then
    printf '  ok   %s\n' "$name"; PASS=$((PASS+1))
  elif [ "$want" = red ] && [ "$rc" -gt 0 ] && grep -qF -- "$needle" <<<"$out"; then
    printf '  ok   %s\n' "$name"; PASS=$((PASS+1))
  else
    printf '  FAIL %s (rc=%s, wanted %s)\n%s\n' "$name" "$rc" "$want" "$out"; FAIL=$((FAIL+1))
  fi
}

check "unmodified catalogue passes" ok "$CATALOG"

# The SLO latency fast-burn rule's runbook_url is api-latency.md; point the
# row's first link at the per-tier page instead — the drift RLT-010 found.
sed -E '/^\| .stellarindex_slo_latency_burn_fast. /s#\[api-latency\]\(runbooks/api-latency\.md\)#[slo-latency-burn-fast](runbooks/slo-latency-burn-fast.md)#' \
  "$CATALOG" >"$TMP"
check "first Runbook link differing from runbook_url is rejected" red "$TMP" \
  "stellarindex_slo_latency_burn_fast: catalogue Runbook column links 'runbooks/slo-latency-burn-fast.md' first"

# A per-alert supplement AFTER the primary link is allowed.
sed -E '/^\| .stellarindex_projector_lag_high. /s#\[projector-lag\]\(runbooks/projector-lag\.md\) \|$#[projector-lag](runbooks/projector-lag.md) + see [projector-replay](runbooks/projector-replay.md) |#' \
  "$CATALOG" >"$TMP"
check "supplement link after the primary is accepted" ok "$TMP"

# A Runbook cell with no link at all cannot be checked, so it fails.
sed -E '/^\| .stellarindex_projector_lag_high. /s#\[projector-lag\]\(runbooks/projector-lag\.md\) \|$#projector-lag |#' \
  "$CATALOG" >"$TMP"
check "Runbook cell with no link is rejected" red "$TMP" \
  "stellarindex_projector_lag_high: catalogue Runbook column links None first"

echo "lint-alerts-catalog-test: $PASS passed, $FAIL failed"
[ "$FAIL" -eq 0 ]
