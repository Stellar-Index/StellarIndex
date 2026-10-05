-- si-apply-scope: operator
--
-- NOT applied by any bootstrap. An operator runs this against an EXISTING
-- deployment (r1) for the DDL below plus the backfill runbook it carries. A
-- FRESH host gets these objects from deploy/clickhouse/tier1_schema.sql,
-- which declares each of them identically, so the DDL here is a mirror and
-- the runbook is the reason the file exists. Scope markers are enforced by
-- scripts/ci/lint-ch-apply-scope.sh.
--
-- movements_by_asset (INV-2140, asset page slice A1): account_movements
-- re-keyed asset-first. Design and column rationale: tier1_schema.sql.
--
-- Step 1 (table + MV, instant) -> Step 2 (partition walk, HEAVY) -> Step 3.

CREATE TABLE IF NOT EXISTS stellar.movements_by_asset
(
    address           String,
    ledger            UInt32,
    ledger_close_time DateTime64(0, 'UTC'),
    tx_hash           String,
    op_index          UInt32,
    leg_index         UInt32,
    direction         LowCardinality(String),
    movement_kind     LowCardinality(String),
    provenance        LowCardinality(String),
    asset             String,
    counterparty      String DEFAULT '',
    amount            Int128,
    attributes        String DEFAULT '{}',
    ingested_at       DateTime DEFAULT now()
)
ENGINE = ReplacingMergeTree(ingested_at)
PARTITION BY intDiv(ledger, 1000000)
ORDER BY (asset, ledger, tx_hash, op_index, leg_index, direction, address);

CREATE MATERIALIZED VIEW IF NOT EXISTS stellar.movements_by_asset_mv
TO stellar.movements_by_asset AS
SELECT address, ledger, ledger_close_time, tx_hash, op_index, leg_index, direction,
       movement_kind, provenance, asset, counterparty, amount, attributes, ingested_at
FROM stellar.account_movements;

-- ── Step 2: partition-at-a-time catch-up ────────────────────────────────────
-- ***Heavy job, r1 only.*** Measured on r1 2026-10-05 (ledgers 60,000,000-
-- 60,009,999, max_threads=4): 8,639,902 rows re-sorted asset-first in 20.3 s
-- = 2.35 us/row. Extrapolated over account_movements' 10,618,524,167 rows:
-- ~6.9 h sort+read, plan 8-14 h with insert cost and sort spill; target
-- 0.4-0.6 TiB compressed. Run AFTER the lake backup and never beside another
-- heavy job. account_movements is ordered by address, so each statement is one
-- 1M-ledger partition (up to ~518M rows) through run-heavy-job.sh.
-- Capture TIP when the MV is created; overlap with the MV era is idempotent
-- under the ReplacingMergeTree, and re-running any window is safe.
--
--   TIP=$(clickhouse-client --port 9300 -q "SELECT max(ledger) FROM stellar.account_movements")
--   for P in $(seq 0 $((TIP / 1000000))); do
--     /usr/local/sbin/run-heavy-job.sh "movements-by-asset-$P" clickhouse-client --port 9300 -q "
--       INSERT INTO stellar.movements_by_asset
--       SELECT address, ledger, ledger_close_time, tx_hash, op_index, leg_index, direction,
--              movement_kind, provenance, asset, counterparty, amount, attributes, ingested_at
--       FROM stellar.account_movements FINAL
--       WHERE ledger >= $((P * 1000000)) AND ledger < $(((P + 1) * 1000000))
--       SETTINGS max_threads = 4, max_memory_usage = 8000000000,
--                max_bytes_before_external_sort = 4000000000"
--     # Let merges drain before the next partition: 100 active parts is a
--     # tenth of parts_to_delay_insert (1000), so inserts never throttle.
--     while [ "$(clickhouse-client --port 9300 -q "SELECT count() FROM system.parts
--         WHERE database = 'stellar' AND table = 'movements_by_asset' AND active")" -ge 100 ]; do
--       sleep 60
--     done
--   done
--
-- FINAL is mandatory: the target key includes asset, so an unmerged stale
-- source row (same key, superseded asset) would be copied in and never merge
-- away.
--
-- OPERATOR NOTE: a re-derive of account_movements that relabels assets (a
-- wrong -network SAC check in ch_cap67_movements.go, or CAP-0038 leg
-- renumbering) leaves the old-asset rows here forever. After such a re-derive,
-- for each affected partition P:
--   ALTER TABLE stellar.movements_by_asset DROP PARTITION P;
-- then repeat that partition's FINAL copy above.
--
-- ── Step 3: verify ──────────────────────────────────────────────────────────
-- Per partition, FINAL row counts and distinct keys (asset included) must match
-- on both sides; a count above the distinct-key count means leftover rows:
--
--   SELECT
--     (SELECT count() FROM stellar.account_movements FINAL
--        WHERE ledger >= 60000000 AND ledger < 61000000) AS src_rows,
--     (SELECT count() FROM stellar.movements_by_asset FINAL
--        WHERE ledger >= 60000000 AND ledger < 61000000) AS dst_rows,
--     (SELECT uniqExact(address, ledger, tx_hash, op_index, leg_index, direction, asset)
--        FROM stellar.account_movements WHERE ledger >= 60000000 AND ledger < 61000000) AS src_keys,
--     (SELECT uniqExact(address, ledger, tx_hash, op_index, leg_index, direction, asset)
--        FROM stellar.movements_by_asset WHERE ledger >= 60000000 AND ledger < 61000000) AS dst_keys
--
-- Expect src_rows = dst_rows and src_keys = dst_keys.
--
-- ── ROLLBACK ────────────────────────────────────────────────────────────────
--   DROP TABLE IF EXISTS stellar.movements_by_asset_mv;
--   DROP TABLE IF EXISTS stellar.movements_by_asset SYNC;
