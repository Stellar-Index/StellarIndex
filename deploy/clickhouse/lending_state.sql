-- si-apply-scope: operator
--
-- NOT applied by any bootstrap. An operator runs this against an EXISTING
-- deployment (r1). A FRESH host gets lending_state_hourly from
-- deploy/clickhouse/tier1_schema.sql, which declares it identically —
-- scripts/ci/lint-ch-apply-scope.sh pins that.
--
-- lending_state_hourly holds, verbatim, the last storage entry of each Blend
-- and BLNT pool/backstop key (ResData, ResConfig, the pool instance,
-- PoolBalance, TierPoolBalance) and of each Comet pool whose LP shares a
-- backstop holds (AllRecordData, TotalShares, the instance; protocol
-- 'comet') in every hour the key changed; entry_xdr '' is a removal.
-- Nothing is decoded on write. Written by
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
-- New keys on a host that already has a watermark (the Comet LP pools: the
-- three curated pools' AllRecordData, TotalShares and instance). The timer
-- captures them from its watermark on; their history below it needs a
-- scoped walk of just those contracts from each one's creation ledger.
-- -lending-contract derives only the named contracts' keys, from -from to
-- the watermark, never moves the watermark or the heartbeat, and arms no
-- interest redo (no Comet pool feeds lending_interest_hourly):
--   1. ch-entry-history -lending-only -print-prefixes  (lists 'lp-pool'
--      contracts and their AllRecordData / TotalShares / Instance keys)
--   2. systemctl stop lending-state.timer  (the walk holds the lock
--      exclusive, and a stopped timer keeps the watermark where the walk
--      ends, so the timer resumes exactly after it)
--   3. run-heavy-job.sh lending-state stellarindex-ops ch-entry-history
--        -lending-only -from 51499546 -write
--        -lending-contract CAS3FL6TLZKDGGSISDBWGGPXT3NRR4DYTZD7YOD3HMYO6LTJUVGRVEAM
--      then the two BLNT pools from their creation:
--      run-heavy-job.sh lending-state stellarindex-ops ch-entry-history
--        -lending-only -from 65134030 -write
--        -lending-contract CDQMIKIHKA5UY3FQJ2UDTLNX3GOPD3GPNZWSPTOBVOHEUMQJQXOPC7Y3,CA5HX3D47PQZWC2E7VWXCKH4534P2NOJEOIXAWU47MI4QV2FQUYY3FLC
--      Each reads every 100,000-ledger window from -from to the watermark
--      for those keys only. The keys change on every swap, join and exit,
--      so expect up to one row per key per active hour: about 20,000
--      AllRecordData and 20,000 TotalShares rows for the Blend pool and
--      under 1,000 per key for each BLNT pool; steady state is at most 48
--      rows per pool-day. A failed walk keeps no cursor: re-run it from the
--      failed window's start.
--   4. systemctl start lending-state.timer
--   5. ch-entry-history -lending-only -prove exits 0; until a pool is
--      walked it says "not walked yet", and the history reader refuses it.
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
-- the replay decision rule). Hold the interest pass first: the delete runs
-- outside the tool's lock, and the pass's lake proof of its seeds is the only
-- guard between the delete and the replay.
--   systemctl stop lending-state.timer   (and no manual interest run)
--   ALTER TABLE stellar.lending_state_hourly
--     DELETE WHERE ledger_seq BETWEEN {lo} AND {hi};
--   (wait for system.mutations), then the replay:
--   ch-entry-history -lending-only -from {lo} -to {hi} -write
--   systemctl start lending-state.timer
-- The replay arms an interest redo from {lo}: lending_interest_hourly must be
-- re-derived from {lo} through the tip (lending_interest.sql) before the
-- interest pass runs anything else.

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
