#!/usr/bin/env bash
# lint-asset-identity.sh — production SQL must not key an asset on one spelling or on a code.
#
# WHY. XLM has three identities (native, crypto:XLM, its SAC) and a classic asset may also trade as
# its SAC; a read keyed on `= 'native'` silently drops the other forms, and `LIKE 'USDC-%'` trusts any
# issuer's USDC. Bind every alias form instead: `col = ANY($n)` with canonical.AssetAliasStrings.
#
# Rejected in non-test Go and SQL: an asset/counterpart column compared to the literal 'native', and
# `LIKE '<CODE>-%'`. A justified site carries `asset-identity: single-form ok (<reason>)` on the line,
# or a row in lint-asset-identity.baseline (path|trimmed line|reason). The baseline is shrink-only: a
# row that no longer matches fails as stale. LINT_ROOT is the test seam.
set -euo pipefail
root="${LINT_ROOT:-$(git rev-parse --show-toplevel)}"
cd "$root"
baseline="${LINT_BASELINE:-scripts/ci/lint-asset-identity.baseline}"
pattern="([A-Za-z_.]*(asset|counterpart)[A-Za-z_]*[[:space:]]*(=|<>|!=)[[:space:]]*'native'|LIKE[[:space:]]+'[A-Za-z0-9]{1,12}-%')"

hits=$(git ls-files -z -- '*.go' '*.sql' \
  | grep -zvE '_test\.go$|^test/|^migrations/' \
  | xargs -0 -r grep -HnE "$pattern" 2>/dev/null \
  | grep -v 'asset-identity: single-form ok' || true)

known=""
if [ -f "$baseline" ]; then
  known=$({ grep -vE '^[[:space:]]*(#|$)' "$baseline" || true; } | awk -F'|' '{print $1 "|" $2}')
fi

fail=0
seen=""
while IFS= read -r hit; do
  [ -z "$hit" ] && continue
  path=${hit%%:*}
  rest=${hit#*:}
  text=${rest#*:}
  text=$(printf '%s' "$text" | sed -e 's/^[[:space:]]*//' -e 's/[[:space:]]*$//')
  key="$path|$text"
  seen="$seen$key"$'\n'
  if ! grep -qxF -- "$key" <<<"$known"; then
    echo "lint-asset-identity: $path:${rest%%:*}: asset keyed on one spelling or a bare code: $text" >&2
    fail=1
  fi
done <<<"$hits"

while IFS= read -r key; do
  [ -z "$key" ] && continue
  if ! grep -qxF -- "$key" <<<"$seen"; then
    echo "lint-asset-identity: stale baseline row (no longer matches; delete it): $key" >&2
    fail=1
  fi
done <<<"$known"

if [ "$fail" -ne 0 ]; then
  echo "Bind every alias form (col = ANY(\$n) with canonical.AssetAliasStrings), or mark a justified line 'asset-identity: single-form ok (<reason>)'." >&2
  exit 1
fi
echo "lint-asset-identity: OK"
