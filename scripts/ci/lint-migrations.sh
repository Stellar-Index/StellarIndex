#!/usr/bin/env bash
# Migration money-column lint (ADR-0003).
#
# Money is NUMERIC — never BIGINT / INT8 / DOUBLE PRECISION / FLOAT /
# REAL (JSON numbers are IEEE-754 doubles; i128 amounts overflow both
# int64 and 2^53). For every migrations/*.up.sql this flags a column
# definition whose name looks monetary next to a non-NUMERIC numeric
# type.
#
# Escape hatch: append `-- lint-money:ok <reason>` on the flagged
# line. Reasons are mandatory — every escape is a design decision
# (e.g. SDEX price_n/price_d, a protocol-defined int32 rational pair
# whose money value lives in the sibling NUMERIC `price` column).
#
# This supersedes the SQL half of scripts/ci/lint-i128.sh (which now
# guards the Go side only); the deep Go-side guard is the go/types
# walk in internal/canonical/i128_truncation_guard_test.go.
#
# Exit 0 clean, non-zero on any violation. Wired into verify.sh + CI.
set -euo pipefail
cd "$(dirname "$0")/../.."
fail=0

# Monetary column-name stems. `_usd` matches only as a suffix of the
# column name (value_usd, volume_usd, …) so `usda`-style codes don't
# trip it. stroop/wei/circulating/market_cap carried over from the
# original lint-i128.sh name set.
name='[a-z0-9_]*(amount|price|supply|balance|volume|reserve|fee|stroop|wei|circulating|market_cap)[a-z0-9_]*|[a-z0-9_]*_usd'
# Non-NUMERIC numeric types that must never hold money.
type='bigint|int8|double precision|float[0-9]*|real'

for f in migrations/*.up.sql; do
  hits=$(grep -nEi "(^|[[:space:](,])\"?(${name})\"?[[:space:]]+(${type})\b" "$f" \
    | grep -vE '^[0-9]+:[[:space:]]*--' \
    | grep -viE -- '-- *lint-money:ok +[^ ]' || true)
  if [ -n "$hits" ]; then
    echo "lint-migrations ❌ ${f}: monetary column is not NUMERIC (ADR-0003):" >&2
    echo "$hits" | sed 's/^/  /' >&2
    fail=1
  fi
  # Stale escapes: a lint-money:ok marker on a line the pattern does
  # not flag is dead weight — the allowlist only shrinks.
  stale=$(grep -nEi -- '-- *lint-money:ok' "$f" \
    | grep -vEi "(^|[[:space:](,])\"?(${name})\"?[[:space:]]+(${type})\b" || true)
  if [ -n "$stale" ]; then
    echo "lint-migrations ❌ ${f}: stale lint-money:ok marker (line no longer matches the lint) — remove it:" >&2
    echo "$stale" | sed 's/^/  /' >&2
    fail=1
  fi
done

if [ "$fail" -eq 0 ]; then
  echo "✅ migration money-column lint passed."
fi
exit "$fail"
