-- si-apply-scope: operator
--
-- NOT applied by any bootstrap. An operator runs this against an EXISTING
-- deployment (r1). A FRESH host gets lending_state_hourly from
-- deploy/clickhouse/tier1_schema.sql, which declares it identically —
-- scripts/ci/lint-ch-apply-scope.sh pins that.
--
-- lending_state_hourly holds, verbatim, the last storage entry of each Blend
-- and BLNT pool/backstop key (ResData, ResConfig, the pool instance,
-- PoolBalance, TierPoolBalance) in every hour the key changed; entry_xdr ''
-- is a removal. Nothing is decoded on write. Written by
-- `stellarindex-ops ch-entry-history -lending-only -write`, which resumes
-- from entry_history_watermark name 'lending_state'.
--
-- Runbook:
--   1. Apply this file.
--   2. stellarindex-ops ch-entry-history -lending-only -print-prefixes
--   3. stellarindex-ops ch-entry-history -lending-only -write
--      (first run walks from the Blend factory genesis to the tip)
--   4. stellarindex-ops ch-entry-history -lending-only -prove
--   5. systemctl enable --now lending-state.timer
--
-- The same run then extends lending_interest_hourly once that pass has been
-- initialised (see lending_interest.sql's runbook); it is keyed, seeded and
-- checked from this table, so it never runs past this table's watermark.
--
-- An hour's row is its last change by (ledger_seq, intra_ledger_seq). A window
-- where that last ledger holds several transactions without intra-ledger order
-- (intra_ledger_seq 0) is refused rather than stored by tx_hash.
--
-- The version is (ledger_seq, insert time), not (ledger_seq, intra_ledger_seq):
-- a replay after a lake re-derive that renumbered a ledger's intra order must
-- displace the old row even when the true last change now has a lower intra.
-- A later ledger still always wins, so an hour split across windows collapses
-- to its last change.
--
-- After a lake re-derive of ledger_entry_changes over [lo, hi] that may have
-- removed changes, delete the range before replaying it (ingest-pipeline.md,
-- the replay decision rule):
--   ALTER TABLE stellar.lending_state_hourly
--     DELETE WHERE ledger_seq BETWEEN {lo} AND {hi};

CREATE TABLE IF NOT EXISTS stellar.lending_state_hourly
(
    contract_id      String,
    protocol         LowCardinality(String),
    key_xdr          String CODEC(ZSTD(3)),
    hour             DateTime('UTC'),
    ledger_seq       UInt32,
    intra_ledger_seq UInt32,
    close_time       DateTime('UTC'),
    entry_xdr        String CODEC(ZSTD(3)),
    version          UInt64 MATERIALIZED bitShiftLeft(toUInt64(ledger_seq), 32) + toUInt32(now())
)
ENGINE = ReplacingMergeTree(version)
PARTITION BY toYYYYMM(hour)
ORDER BY (contract_id, key_xdr, hour);

-- Converges a table created with the (ledger_seq, intra_ledger_seq) version.
-- Metadata only: existing rows keep their stored version, which any new row
-- at the same ledger outranks (an insert time exceeds every intra_ledger_seq).
ALTER TABLE stellar.lending_state_hourly
    MODIFY COLUMN version UInt64 MATERIALIZED bitShiftLeft(toUInt64(ledger_seq), 32) + toUInt32(now());
