#!/usr/bin/env bash
# ch-schema-drift-test.sh — self-test for ch-schema-drift.sh.
#
# Mirrors scripts/ops/ch-schema-snapshot-test.sh: a hermetic harness that
# feeds the checker synthetic inputs and asserts the verdict, so the
# comparison logic is exercised without a ClickHouse server.
#
# The fixtures matter as much as the assertions. LIVE_OK below is written
# in ClickHouse's OWN `SHOW CREATE TABLE` rendering — backticked column
# names, the database qualifier, an inferred column list on the
# materialized view, a trailing `SETTINGS index_granularity = 8192`, a
# tuple ORDER BY, and the newline layout the server actually emits —
# while the intent fixture is written the way a human writes
# tier1_schema.sql. If the normalizer only ever saw two files of the same
# shape (which a naive "diff the file against itself" test does), it
# would pass while being useless against a real snapshot.
#
# Usage: ./configs/ansible/roles/archival-node/files/ch-schema-drift-test.sh
set -uo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
drift="$here/ch-schema-drift.sh"
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

pass=0
fail=0

# bt is a literal backtick — ClickHouse quotes identifiers with them in
# SHOW CREATE output, and building the sed patterns from a variable keeps
# them out of any quoting context that would try to evaluate them.
bt='`'

indent() { while IFS= read -r l; do printf '       | %s\n' "$l"; done <<<"$1"; }

# run <name> <want-rc> <intent-file> <live-file>
run() {
  local name="$1" want="$2" intent="$3" live="$4" out rc
  out="$(INTENT="$intent" LIVE_SCHEMA="$live" TEXTFILE_DIR=/dev/null \
         bash "$drift" 2>&1)"
  rc=$?
  if [[ "$rc" -eq "$want" ]]; then
    pass=$((pass + 1))
    echo "ok   — $name (rc=$rc)"
  else
    fail=$((fail + 1))
    echo "FAIL — $name: rc=$rc, want $want"
    indent "$out"
  fi
}

# expect_msg <name> <substring> <intent> <live>
expect_msg() {
  local name="$1" needle="$2" intent="$3" live="$4" out
  out="$(INTENT="$intent" LIVE_SCHEMA="$live" TEXTFILE_DIR=/dev/null \
         bash "$drift" 2>&1)"
  if grep -qF -- "$needle" <<<"$out"; then
    pass=$((pass + 1))
    echo "ok   — $name (reported '$needle')"
  else
    fail=$((fail + 1))
    echo "FAIL — $name: output did not name '$needle'"
    indent "$out"
  fi
}

# ─── fixtures ───────────────────────────────────────────────────────

cat > "$tmp/intent.sql" <<'SQL'
-- Founding DDL, written the way a human writes it.
CREATE DATABASE IF NOT EXISTS stellar;

CREATE TABLE IF NOT EXISTS stellar.ledgers
(
    ledger_seq   UInt32,
    close_time   DateTime('UTC'),
    ledger_hash  String,
    ingested_at  DateTime DEFAULT now()
)
ENGINE = ReplacingMergeTree(ingested_at)
PARTITION BY intDiv(ledger_seq, 1000000)
ORDER BY ledger_seq;

CREATE TABLE IF NOT EXISTS stellar.account_movements
(
    address       String,
    ledger        UInt32,
    tx_hash       String,
    op_index      UInt32,
    amount        Int128,
    ingested_at   DateTime DEFAULT now(),
    INDEX idx_am_tx tx_hash TYPE bloom_filter(0.01) GRANULARITY 1
)
ENGINE = ReplacingMergeTree(ingested_at)
PARTITION BY intDiv(ledger, 1000000)
ORDER BY (address, ledger, tx_hash, op_index);

CREATE MATERIALIZED VIEW IF NOT EXISTS stellar.tx_hash_index_mv
TO stellar.tx_hash_index AS
SELECT tx_hash, ledger_seq
FROM stellar.transactions;
SQL

# ClickHouse's own SHOW CREATE rendering of an equivalent live schema.
cat > "$tmp/live-ok.sql" <<'SQL'
CREATE DATABASE IF NOT EXISTS stellar;

CREATE TABLE stellar.ledgers
(
    `ledger_seq` UInt32,
    `close_time` DateTime('UTC'),
    `ledger_hash` String,
    `ingested_at` DateTime DEFAULT now()
)
ENGINE = ReplacingMergeTree(ingested_at)
PARTITION BY intDiv(ledger_seq, 1000000)
ORDER BY ledger_seq
SETTINGS index_granularity = 8192;

CREATE TABLE stellar.account_movements
(
    `address` String,
    `ledger` UInt32,
    `tx_hash` String,
    `op_index` UInt32,
    `amount` Int128,
    `ingested_at` DateTime DEFAULT now(),
    INDEX idx_am_tx tx_hash TYPE bloom_filter(0.01) GRANULARITY 1
)
ENGINE = ReplacingMergeTree(ingested_at)
PARTITION BY intDiv(ledger, 1000000)
ORDER BY (address, ledger, tx_hash, op_index)
SETTINGS index_granularity = 8192;

CREATE MATERIALIZED VIEW stellar.tx_hash_index_mv TO stellar.tx_hash_index
(
    `tx_hash` String,
    `ledger_seq` UInt32
) AS
SELECT
    tx_hash,
    ledger_seq
FROM stellar.transactions;
SQL

mutate() { sed "$1" "$tmp/live-ok.sql" > "$2"; }

# ─── the clean case ─────────────────────────────────────────────────
run "clean: SHOW CREATE rendering of the same schema" 0 "$tmp/intent.sql" "$tmp/live-ok.sql"

# ─── each compared attribute, mutated one at a time ─────────────────
mutate 's/ORDER BY (address, ledger, tx_hash, op_index)/ORDER BY (address, ledger, op_index, tx_hash)/' "$tmp/live-order.sql"
run       "drift: ORDER BY reordered live"        1 "$tmp/intent.sql" "$tmp/live-order.sql"
expect_msg "drift: names the ORDER BY" "DRIFT account_movements.order" "$tmp/intent.sql" "$tmp/live-order.sql"

mutate 's/PARTITION BY intDiv(ledger, 1000000)/PARTITION BY intDiv(ledger, 5000000)/' "$tmp/live-part.sql"
run       "drift: PARTITION BY changed live"      1 "$tmp/intent.sql" "$tmp/live-part.sql"
expect_msg "drift: names the PARTITION BY" "DRIFT account_movements.partition" "$tmp/intent.sql" "$tmp/live-part.sql"

# ReplacingMergeTree version column swapped: same engine NAME, different
# dedup semantics. system.tables.engine would NOT catch this — the engine
# args only exist in SHOW CREATE, which is why the checker parses that.
sed 's/^ENGINE = ReplacingMergeTree(ingested_at)$/ENGINE = ReplacingMergeTree(ledger)/' "$tmp/live-ok.sql" > "$tmp/live-eng.sql"
run       "drift: ReplacingMergeTree version column changed" 1 "$tmp/intent.sql" "$tmp/live-eng.sql"
expect_msg "drift: names the engine" "DRIFT ledgers.engine" "$tmp/intent.sql" "$tmp/live-eng.sql"

sed "/${bt}op_index${bt} UInt32,/d" "$tmp/live-ok.sql" > "$tmp/live-col.sql"
run       "drift: column dropped live"            1 "$tmp/intent.sql" "$tmp/live-col.sql"
expect_msg "drift: names the columns" "DRIFT account_movements.columns" "$tmp/intent.sql" "$tmp/live-col.sql"

# A whole table never created on the box.
awk '/^CREATE TABLE stellar.account_movements/{skip=1} skip && /^SETTINGS index_granularity/{skip=0; next} !skip' \
  "$tmp/live-ok.sql" > "$tmp/live-missing.sql"
run       "drift: declared table absent live"     1 "$tmp/intent.sql" "$tmp/live-missing.sql"
expect_msg "drift: names the missing table" "ABSENT from the live schema" "$tmp/intent.sql" "$tmp/live-missing.sql"

# MV repointed at a different destination table.
sed 's/TO stellar.tx_hash_index$/TO stellar.tx_hash_index_v2/' "$tmp/live-ok.sql" > "$tmp/live-mv.sql"
run       "drift: materialized view repointed"    1 "$tmp/intent.sql" "$tmp/live-mv.sql"

# ─── the non-fatal directions ───────────────────────────────────────
# A live table absent from the founding DDL is counted, not failed.
cat "$tmp/live-ok.sql" > "$tmp/live-extra.sql"
cat >> "$tmp/live-extra.sql" <<'SQL'

CREATE TABLE stellar.holders_snapshot
(
    `address` String,
    `balance` Int128
)
ENGINE = ReplacingMergeTree
ORDER BY address
SETTINGS index_granularity = 8192;
SQL
run       "uncodified live table is not a failure" 0 "$tmp/intent.sql" "$tmp/live-extra.sql"
expect_msg "uncodified live table is reported"     "UNCODIFIED holders_snapshot" "$tmp/intent.sql" "$tmp/live-extra.sql"

# Type text re-rendered by ClickHouse is INFO, never drift.
sed "s/${bt}amount${bt} Int128,/${bt}amount${bt} Int128 CODEC(Delta, ZSTD(3)),/" "$tmp/live-ok.sql" > "$tmp/live-codec.sql"
run "type/CODEC re-rendering is not drift" 0 "$tmp/intent.sql" "$tmp/live-codec.sql"

# ─── cannot-compare must NOT read as clean ──────────────────────────
run "unreadable live schema exits 2, not 0" 2 "$tmp/intent.sql" "$tmp/does-not-exist.sql"
: > "$tmp/empty.sql"
run "empty live schema exits 2, not 0"      2 "$tmp/intent.sql" "$tmp/empty.sql"
run "empty intent exits 2, not 0"           2 "$tmp/empty.sql"  "$tmp/live-ok.sql"

# ─── the real repo intent must at least parse ───────────────────────
real_intent="$here/../../../../../deploy/clickhouse/tier1_schema.sql"
if [[ -r "$real_intent" ]]; then
  run "real tier1_schema.sql compares clean against itself" 0 \
    "$real_intent" "$real_intent"
fi

echo
echo "ch-schema-drift-test: $pass passed, $fail failed"
[[ "$fail" -eq 0 ]]
