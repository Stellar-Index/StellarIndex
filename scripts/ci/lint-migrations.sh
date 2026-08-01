#!/usr/bin/env bash
# Migration lint: money-column (ADR-0003) + file integrity (audit C4-7).
#
# Two passes, both gating (exit non-zero on any violation):
#   1. money-column — monetary columns must be NUMERIC (ADR-0003).
#   2. file integrity — every NNNN_*.up.sql has a matching NON-EMPTY
#      *.down.sql (and no orphan downs), no duplicate NNNN prefixes, and
#      no empty files. Numbering GAPS are a non-fatal WARNING (this repo
#      legitimately skips numbers when a migration is squashed/removed).
#
# ── money-column detail ──
#
# Money is NUMERIC — never BIGINT / INTEGER / SMALLINT / INT8 / INT4 /
# INT2 / INT / MONEY / DOUBLE PRECISION / FLOAT[n] / REAL (JSON numbers
# are IEEE-754 doubles; i128 amounts overflow both int64 and 2^53, and a
# narrower integer/money type loses even more). For every
# migrations/*.up.sql this flags a column definition whose name looks
# monetary next to a non-NUMERIC numeric type.
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
# original lint-i128.sh name set; twap/vwap/tvl/wealth added 2026-08
# (a time/volume-weighted-average PRICE and total-value-locked / net-
# worth aggregate are money — must be NUMERIC, never float).
#
# Deliberately NOT in the stem set (each would false-positive on
# legitimate existing NON-money columns, and a stem that flags real
# code is worse than the gap it closes):
#   - median / mad — the volatility_baseline columns hold a *median
#     bucket-to-bucket VWAP percent-change return* + its MAD (0007/
#     0008): dimensionless statistics, correctly DOUBLE PRECISION, not
#     amounts. There is no genuine money-median column in the tree.
#   - rate — money rates already match via the `_usd` suffix rule
#     (rate_usd); bare `rate` would flag rate_limit_per_min (a request
#     count, 0027).
#   - cap — market cap already matches via `market_cap`; bare `cap`
#     would flag capacity/capture/…-style names.
name='[a-z0-9_]*(amount|price|supply|balance|volume|reserve|fee|stroop|wei|circulating|market_cap|twap|vwap|tvl|wealth)[a-z0-9_]*|[a-z0-9_]*_usd'
# Non-NUMERIC numeric types that must never hold money. Both the
# 128-bit-overflowing wide ints (bigint/int8) AND the narrower ints
# (integer/smallint/int4/int2/int) AND floats (double precision/float[n]
# /real) AND `money` (a fixed 2-decimal locale-dependent type — never
# correct for an i128 amount). NUMERIC is the only allowed home.
type='bigint|integer|smallint|int8|int4|int2|int|money|double precision|float[0-9]*|real'

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

# ─── Pass 2: pairing / numbering / non-empty (audit C4-7) ───
# A missing or empty .down.sql means a migration can't be rolled back —
# a silent operational trap discovered only during an incident. This
# pass makes it a CI failure. Gaps are WARN-only (see header): the tree
# legitimately skips numbers (e.g. 0075, 0077-0079, 0084 today) when a
# migration is squashed out, so a hard no-gap rule would false-positive.

# Non-empty: every migration file must have content.
for f in migrations/*.sql; do
  [ -e "$f" ] || continue
  if [ ! -s "$f" ]; then
    echo "lint-migrations ❌ ${f}: migration file is empty" >&2
    fail=1
  fi
done

# up ↔ down pairing: every up needs a non-empty down; no orphan downs.
for up in migrations/*.up.sql; do
  [ -e "$up" ] || continue
  down="${up%.up.sql}.down.sql"
  if [ ! -f "$down" ]; then
    echo "lint-migrations ❌ ${up}: no matching down migration (${down##*/})" >&2
    fail=1
  elif [ ! -s "$down" ]; then
    echo "lint-migrations ❌ ${down}: down migration is empty (a down must reverse its up)" >&2
    fail=1
  fi
done
for down in migrations/*.down.sql; do
  [ -e "$down" ] || continue
  up="${down%.down.sql}.up.sql"
  if [ ! -f "$up" ]; then
    echo "lint-migrations ❌ ${down}: orphan down migration (no matching ${up##*/})" >&2
    fail=1
  fi
done

# Duplicate NNNN prefixes among *.up.sql (two migrations claiming one number).
dupes=$(ls migrations/*.up.sql 2>/dev/null \
  | sed -E 's#.*/([0-9]+)_.*#\1#' | sort | uniq -d || true)
if [ -n "$dupes" ]; then
  echo "lint-migrations ❌ duplicate migration number(s) among *.up.sql:" >&2
  echo "$dupes" | sed 's/^/  /' >&2
  fail=1
fi

# Numbering gaps → WARNING only (non-fatal; see header).
gaps=$(ls migrations/*.up.sql 2>/dev/null \
  | sed -E 's#.*/([0-9]+)_.*#\1#' | sort -n | awk '
    NR==1 { prev = $1 + 0; next }
    { cur = $1 + 0; while (prev + 1 < cur) { prev++; printf "%04d ", prev } prev = cur }
  ' || true)
if [ -n "$gaps" ]; then
  echo "lint-migrations ⚠️  numbering gap(s) (non-fatal — squashed/removed migrations): ${gaps}" >&2
fi

if [ "$fail" -eq 0 ]; then
  echo "✅ migration lint passed (money-column + pairing/numbering/non-empty)."
fi
exit "$fail"
