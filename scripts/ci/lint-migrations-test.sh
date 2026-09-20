#!/usr/bin/env bash
# lint-migrations-test.sh — fixture tests for the ClickHouse money-column
# pass of scripts/ci/lint-migrations.sh (ADR-0003 on the lake DDL).
#
# The pass exists because the money-column gate globbed only
# migrations/*.up.sql: a `volume_usd Float64` dropped into
# deploy/clickhouse/ was invisible to every lint while the same column in
# a Postgres migration was a CI failure. The verdicts pinned here are the
# shapes that defect takes and the ways the guard could go vacuous:
#
#   - the REAL tree passes and reports how many files it inspected;
#   - a NEW Float64 money column in a CREATE TABLE is CAUGHT, keyed to
#     its table.column;
#   - a Nullable(Float64) money column added by ALTER TABLE is CAUGHT;
#   - exact integer and Decimal/String money columns, and a float whose
#     name merely contains a stem (usda_ratio), are NOT flagged;
#   - a reasoned `-- lint-money:ok` escape clears a column; a stale one
#     is CAUGHT;
#   - a baseline entry whose column is no longer a float is CAUGHT (the
#     baseline only shrinks);
#   - an empty directory FAILS rather than passing vacuously.
#
# The Postgres passes run against the real migrations/ tree in every case,
# so this file assumes (and the first case asserts) that tree is clean.
#
# Run: bash scripts/ci/lint-migrations-test.sh
set -uo pipefail

cd "$(dirname "$0")/../.." || exit 1
LINT="$PWD/scripts/ci/lint-migrations.sh"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
pass=0
fail=0

indent() { printf '%s\n' "         ${1//$'\n'/$'\n'         }"; }

run() { CH_DIR="$1" bash "$LINT" 2>&1; }

clean() { # clean <desc> <ch_dir>
  local desc="$1" dir="$2" out got
  out="$(run "$dir")"; got=$?
  if [ "$got" -eq 0 ]; then
    echo "  ok   $desc"; pass=$((pass + 1))
  else
    echo "  FAIL $desc (exit $got, want 0)"; indent "$out"
    fail=$((fail + 1))
  fi
}

catches() { # catches <desc> <ch_dir> <needle> — must fail AND say why.
  local desc="$1" dir="$2" needle="$3" out got
  out="$(run "$dir")"; got=$?
  if [ "$got" -eq 0 ]; then
    echo "  FAIL $desc (exit 0, want non-zero)"; fail=$((fail + 1)); return
  fi
  case "$out" in
    *"$needle"*) : ;;
    *) echo "  FAIL $desc (failed, but no finding matching: $needle)"
       indent "$out"
       fail=$((fail + 1)); return ;;
  esac
  echo "  ok   $desc"; pass=$((pass + 1))
}

mk() { # mk <name> <file> <<sql  -> echoes the dir
  local dir="$TMP/$1"
  mkdir -p "$dir"
  cat > "$dir/$2"
  echo "$dir"
}

echo "lint-migrations-test: ClickHouse money-column pass"

clean "real tree passes" "deploy/clickhouse"
out="$(run deploy/clickhouse)"
case "$out" in
  *"ClickHouse money pass inspected "[1-9]*) echo "  ok   real tree reports a non-zero file count"; pass=$((pass + 1)) ;;
  *) echo "  FAIL real tree did not report its file count"; indent "$out"; fail=$((fail + 1)) ;;
esac

d="$(mk float-create rollup.sql <<'SQL'
-- si-apply-scope: operator
CREATE TABLE IF NOT EXISTS stellar.asset_month_prices
(
    asset      String,
    volume_usd Float64
)
ENGINE = MergeTree ORDER BY asset;
SQL
)"
catches "Float64 money column in CREATE TABLE is caught, keyed to its table" "$d" \
  "money column stellar.asset_month_prices.volume_usd is a float"

d="$(mk float-alter add.sql <<'SQL'
-- si-apply-scope: operator
ALTER TABLE stellar.transactions
    ADD COLUMN IF NOT EXISTS fee_usd Nullable(Float64) DEFAULT 0;
SQL
)"
catches "Nullable(Float64) money column added by ALTER TABLE is caught" "$d" \
  "money column stellar.transactions.fee_usd is a float"

d="$(mk exact ok.sql <<'SQL'
-- si-apply-scope: fresh-host
-- volume_usd Float64 in a comment is not a column.
CREATE TABLE IF NOT EXISTS stellar.positions
(
    amount      Int128,
    balance     Int64 DEFAULT 0,
    base_fee    UInt32,
    price       String,
    vwap_usd    Decimal(38, 7),
    holders     UInt64,
    usda_ratio  Float64
)
ENGINE = MergeTree ORDER BY amount;
SQL
)"
clean "exact integer, Decimal and String money columns pass; a non-money float passes" "$d"

d="$(mk escaped esc.sql <<'SQL'
-- si-apply-scope: operator
CREATE TABLE IF NOT EXISTS stellar.cohort
(
    amount Float64 -- lint-money:ok display magnitude, never settled
)
ENGINE = MergeTree ORDER BY amount;
SQL
)"
clean "a reasoned lint-money:ok escape clears a float money column" "$d"

d="$(mk stale-escape stale.sql <<'SQL'
-- si-apply-scope: operator
CREATE TABLE IF NOT EXISTS stellar.cohort
(
    holders UInt64 -- lint-money:ok nothing to escape here
)
ENGINE = MergeTree ORDER BY holders;
SQL
)"
catches "a lint-money:ok marker on a line the lint does not flag is caught" "$d" \
  "stale lint-money:ok marker"

d="$(mk stale-baseline account_cohort_rollup.sql <<'SQL'
-- si-apply-scope: operator
CREATE TABLE IF NOT EXISTS stellar.asset_month_usd_prices
(
    volume_usd Decimal(38, 7)
)
ENGINE = MergeTree ORDER BY volume_usd;
CREATE TABLE IF NOT EXISTS stellar.account_cohort_positions
(
    amount Float64
)
ENGINE = MergeTree ORDER BY amount;
SQL
)"
catches "a baseline entry whose column was retyped is caught as stale" "$d" \
  "stale ch_float_baseline entry account_cohort_rollup.sql:stellar.asset_month_usd_prices.volume_usd"

mkdir -p "$TMP/empty"
catches "an empty directory fails rather than passing vacuously" "$TMP/empty" \
  "cannot pass vacuously"

echo "lint-migrations-test: ${pass} passed, ${fail} failed"
[ "$fail" -eq 0 ]
