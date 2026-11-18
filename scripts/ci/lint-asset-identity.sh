#!/usr/bin/env bash
# lint-asset-identity.sh — production SQL must not key an asset on one spelling or on a code.
#
# WHY. XLM has three identities (native, crypto:XLM, its SAC) and a classic asset may also trade as
# its SAC; a read keyed on `= 'native'` silently drops the other forms, and `LIKE 'USDC-%'` trusts any
# issuer's USDC. Bind every alias form instead: `col = ANY($n)` with canonical.AssetAliasStrings.
#
# Rejected in non-test Go and SQL: an asset/counterpart column compared (=, ==, <>, !=, either operand
# order, or a one-element [NOT] IN) to the literal 'native' or 'crypto:XLM'; a code column compared to a
# bare code ('USDC'); and a code-prefix match (LIKE/ILIKE '<CODE>-%' in any case, starts_with,
# strings.HasPrefix "<CODE>-"). NOT covered: a single bound parameter (`asset = $1`) — the lint cannot
# see whether the caller bound one spelling or looped the aliases; that is review's job.
# A justified site carries `asset-identity: single-form ok (<reason>)` on the line, or a row in
# lint-asset-identity.baseline (path|trimmed line|reason). The baseline is shrink-only: a row that no
# longer matches fails as stale. LINT_ROOT is the test seam.
set -euo pipefail
root="${LINT_ROOT:-$(git rev-parse --show-toplevel)}"
cd "$root"
baseline="${LINT_BASELINE:-scripts/ci/lint-asset-identity.baseline}"
# Literals are SQL single-quoted: Go's `x.Code == "USD"` on a fiat leg is identity, not a code key.
col='[A-Za-z_.]*(asset|counterpart)[A-Za-z_]*'
op='(==|=|<>|!=)'
lit="'(native|crypto:XLM)'"
codecol='(^|[^A-Za-z_])([A-Za-z_]+\.)?([Aa]sset_?)?[Cc]ode'
code="'[A-Z0-9]{1,12}'"
pattern="${col}[[:space:]]*${op}[[:space:]]*${lit}"
pattern+="|${lit}[[:space:]]*${op}[[:space:]]*${col}"
pattern+="|${col}[[:space:]]+([Nn][Oo][Tt][[:space:]]+)?[Ii][Nn][[:space:]]*\\([[:space:]]*${lit}[[:space:]]*\\)"
pattern+="|[Ii]?[Ll][Ii][Kk][Ee][[:space:]]+'[A-Za-z0-9]{1,12}-%'"
pattern+="|[Ss][Tt][Aa][Rr][Tt][Ss]_[Ww][Ii][Tt][Hh][[:space:]]*\\([^)]*'[A-Za-z0-9]{1,12}-'"
pattern+="|HasPrefix\\([^)]*\"[A-Z0-9]{1,12}-\""
pattern+="|${codecol}[[:space:]]*${op}[[:space:]]*${code}"
pattern+="|${code}[[:space:]]*${op}[[:space:]]*${codecol}"

# git grep exits 1 on no match; anything else is an error that must not read as clean.
rc=0
raw=$(git grep -nE "$pattern" -- '*.go' '*.sql' ':!*_test.go' ':!test/' ':!migrations/') || rc=$?
if [ "$rc" -gt 1 ]; then
  echo "lint-asset-identity: git grep failed (exit $rc); the lint did not run" >&2
  exit 2
fi
hits=$(grep -v 'asset-identity: single-form ok' <<<"$raw" || true)

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
