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
# It also pins the hypertable-index pass (the 0150 shape — a bare
# in-transaction CREATE INDEX on an existing hypertable — is CAUGHT; IF
# NOT EXISTS under SET LOCAL lock_timeout passes) and the CAGG
# re-materialization pass (a recreate WITH NO DATA that names no refresh
# for a dropped view — the 0115/0147 down shape — is CAUGHT).
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

run() { CH_DIR="${1:-deploy/clickhouse}" MIG_DIR="${2:-migrations}" TXN_DIR="${TXN:-migrations}" bash "$LINT" 2>&1; }

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

# Passes 6 and 7 read MIG_DIR (run's second argument); the passes above keep reading migrations/
mig() { # mig <name> <file> <<sql — a fixture tree whose 0001 creates the trades hypertable
  local dir
  dir="$(mk "$1" "$2")"
  printf '%s\n' "CREATE TABLE trades (ts timestamptz NOT NULL, signer text);" \
    "SELECT create_hypertable('trades', 'ts');" > "$dir/0001_trades.up.sql"
  echo "$dir"
}
clean_mig() { # clean_mig <desc> <mig_dir>
  local out got
  out="$(run deploy/clickhouse "$2")"; got=$?
  if [ "$got" -eq 0 ]; then echo "  ok   $1"; pass=$((pass + 1))
  else echo "  FAIL $1 (exit $got, want 0)"; indent "$out"; fail=$((fail + 1)); fi
}
mcatches() { # mcatches <desc> <mig_dir> <needle>
  local out got
  out="$(run deploy/clickhouse "$2")"; got=$?
  if [ "$got" -ne 0 ] && [[ "$out" == *"$3"* ]]; then echo "  ok   $1"; pass=$((pass + 1))
  else echo "  FAIL $1 (exit $got, want non-zero naming: $3)"; indent "$out"; fail=$((fail + 1)); fi
}

echo "lint-migrations-test: hypertable-index pass"

out="$(run)"
case "$out" in
  *"hypertable-index pass checked "[1-9]*"CAGG re-materialization pass inspected "[1-9]*) echo "  ok   real tree reports non-zero hypertable-index and CAGG counts"; pass=$((pass + 1)) ;;
  *) echo "  FAIL real tree did not report its hypertable-index / CAGG counts"; indent "$out"; fail=$((fail + 1)) ;;
esac

d="$(mig bare-index 0002_signer.up.sql <<'SQL'
BEGIN;
ALTER TABLE trades ADD COLUMN signer text;
CREATE INDEX trades_signer_idx ON trades (signer)
    WHERE signer IS NOT NULL;
COMMIT;
SQL
)"
mcatches "the 0150 shape (no IF NOT EXISTS, no lock_timeout) on a hypertable is caught" "$d" \
  "CREATE INDEX trades_signer_idx ON trades lacks IF NOT EXISTS and SET LOCAL lock_timeout"

d="$(mig no-timeout 0002_signer.up.sql <<'SQL'
BEGIN;
CREATE INDEX IF NOT EXISTS trades_signer_idx ON trades (signer);
COMMIT;
SQL
)"
mcatches "IF NOT EXISTS without a lock_timeout is caught" "$d" \
  "CREATE INDEX trades_signer_idx ON trades lacks SET LOCAL lock_timeout"

d="$(mig conforming 0002_signer.up.sql <<'SQL'
-- Pre-build by hand first: CREATE INDEX CONCURRENTLY ... (0037 recipe).
BEGIN;
SET LOCAL lock_timeout = '5s';
CREATE INDEX IF NOT EXISTS trades_signer_idx
    ON trades (signer) WHERE signer IS NOT NULL;
COMMIT;
SQL
)"
clean_mig "IF NOT EXISTS under SET LOCAL lock_timeout passes, split across lines" "$d"

d="$(mig fresh-table 0002_events.up.sql <<'SQL'
CREATE TABLE soroban_things (ts timestamptz NOT NULL, id text);
SELECT create_hypertable('soroban_things', 'ts');
CREATE INDEX soroban_things_id_idx ON soroban_things (id);
CREATE INDEX plain_idx ON not_a_hypertable (id);
SQL
)"
clean_mig "an index on a hypertable born in the same file, or on a plain table, passes" "$d"

mkdir -p "$TMP/no-hyper"
printf 'CREATE TABLE t (id int);\n' > "$TMP/no-hyper/0001_t.up.sql"
mcatches "a tree with no create_hypertable fails rather than passing vacuously" "$TMP/no-hyper" \
  "hypertable-index pass has gone vacuous"

echo "lint-migrations-test: CAGG re-materialization pass"

d="$(mig cagg-no-refresh 0002_recreate.down.sql <<'SQL'
-- ⚠ this leaves the CAGGs EMPTY; see the up.
BEGIN;
DROP MATERIALIZED VIEW IF EXISTS twap_1h;  -- migration-compat:ok restore
DROP MATERIALIZED VIEW IF EXISTS prices_1m;
CREATE MATERIALIZED VIEW prices_1m WITH (timescaledb.continuous) AS
SELECT time_bucket('1 minute', ts) AS bucket FROM trades GROUP BY 1
WITH NO DATA;
COMMIT;
SQL
)"
mcatches "a down that recreates CAGGs WITH NO DATA and names no refresh is caught (the 0115/0147 down shape)" "$d" \
  "names no refresh for: prices_1m twap_1h"

d="$(mig cagg-partial 0002_recreate.up.sql <<'SQL'
--   CALL refresh_continuous_aggregate('prices_1m', NULL, now());
BEGIN;
DROP MATERIALIZED VIEW twap_1h, prices_1m CASCADE;
CREATE MATERIALIZED VIEW prices_1m WITH (timescaledb.continuous) AS
SELECT time_bucket('1 minute', ts) AS bucket FROM trades GROUP BY 1
WITH NO DATA;
COMMIT;
SQL
)"
mcatches "a multi-view DROP names every view, and one missing refresh is caught" "$d" \
  "names no refresh for: twap_1h"

d="$(mig cagg-named 0002_recreate.up.sql <<'SQL'
--   CALL refresh_continuous_aggregate('prices_1m', NULL, now());
--   CALL refresh_continuous_aggregate('twap_1h', NULL, now());
BEGIN;
DROP MATERIALIZED VIEW IF EXISTS twap_1h;
DROP MATERIALIZED VIEW IF EXISTS prices_1m;
CREATE MATERIALIZED VIEW prices_1m WITH (timescaledb.continuous) AS
SELECT time_bucket('1 minute', ts) AS bucket FROM trades GROUP BY 1
WITH NO DATA;
COMMIT;
SQL
)"
clean_mig "a recreate that names every view's refresh passes" "$d"

d="$(mig cagg-initial 0002_create.up.sql <<'SQL'
CREATE MATERIALIZED VIEW prices_1m WITH (timescaledb.continuous) AS
SELECT time_bucket('1 minute', ts) AS bucket FROM trades GROUP BY 1
WITH NO DATA;
SQL
)"
clean_mig "an initial CAGG creation (nothing dropped, nothing emptied) passes" "$d"

echo "lint-migrations-test: atomicity pass"

TXN="$(mk txn-tail 0900_tail.up.sql <<'SQL'
-- header
BEGIN;
ALTER TABLE t SET (timescaledb.compress = false);
COMMIT;

-- restore
ALTER TABLE t SET (timescaledb.compress);
SQL
)"
catches "SQL after COMMIT is caught with its line" deploy/clickhouse \
  "0900_tail.up.sql:7: SQL after the file's COMMIT/ROLLBACK"

TXN="$(mk txn-lower 0901_lower.down.sql <<'SQL'
begin;
select 1;
commit work;
select 2;
SQL
)"
catches "a lower-case COMMIT WORK followed by SQL is caught" deploy/clickhouse \
  "0901_lower.down.sql:4:"

TXN="$(mk txn-ok 0902_ok.up.sql <<'SQL'
-- a COMMIT; in a comment is not a statement
BEGIN;
DO $$
BEGIN
    PERFORM 1;
END
$$;
COMMIT;
-- trailing comments are fine
SQL
)"
clean "a fully wrapped body with a DO block and trailing comments passes" deploy/clickhouse

TXN="$TMP/empty"
catches "an empty migrations directory fails rather than passing vacuously" deploy/clickhouse \
  "atomicity pass cannot pass vacuously"
unset TXN

out="$(run deploy/clickhouse)"
case "$out" in
  *"atomicity pass inspected "[1-9]*) echo "  ok   real tree reports a non-zero atomicity file count"; pass=$((pass + 1)) ;;
  *) echo "  FAIL real tree did not report its atomicity file count"; indent "$out"; fail=$((fail + 1)) ;;
esac

echo "lint-migrations-test: register row shape"

# Fixture registers are the real one with a single row damaged, so the
# presence checks stay clean and only the shape check can fire.
reg_with() { # reg_with <name> <number> <cell> -> echoes the fixture path
  local out="$TMP/$1.md"
  NUM="$2" CELL="$3" awk '
    index($0, "| " ENVIRON["NUM"] " | ") == 1 {
      p = index(substr($0, 10), " | ") + 11
      print substr($0, 1, p) ENVIRON["CELL"] " |"; next
    }
    { print }' migrations/README.md > "$out"
  echo "$out"
}
REGISTER="$(reg_with head-cut 0016 'Persists the pair_contract → (token0, token1) mapping that the')" \
  catches "a row cut off mid-sentence is caught" deploy/clickhouse \
  "0016 (does not end in a full stop"
REGISTER="$(reg_with tail-cut 0060 '(F-1324).')" \
  catches "a row holding only a sentence's tail is caught" deploy/clickhouse \
  "0060 (does not start a sentence"
REGISTER="$(reg_with bold-end 0016 '**Operator warning ends in bold.**')" \
  clean "a row ending in a bold full stop passes" deploy/clickhouse

echo "lint-migrations-test: ${pass} passed, ${fail} failed"
[ "$fail" -eq 0 ]
