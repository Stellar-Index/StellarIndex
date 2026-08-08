-- contract_active_ledgers — per-(contract, ledger) activity index for the
-- explorer's contract detail read (site audit 2026-08-07/08).
--
-- WHY: stellar.contract_events is ORDER BY (ledger_seq, tx_hash, …). The
-- "most recent N events for contract X" read relies on a reverse
-- read-in-order walk (v0.26.1) that is fast for BUSY contracts (tail dense
-- with matches) but scans the whole key range backwards for QUIET ones —
-- measured avg 9.3s / 114M rows on r1, and every such cold page 503s at the
-- 8s explorer budget (three user reports: CCW5IBJ7…, CBUYWJCO…, CDMAFG3H…).
--
-- THE INDEX: one narrow row per (contract, ledger-with-activity). The reader
-- walks the contract's most recent active ledgers here (primary-key reverse
-- walk, µs), then reads the events ledger-pruned from contract_events.
-- ~20-40 GiB against 12.7B events (contract_id compresses to ~nothing
-- sorted; ledger_seq/close_time delta-compress) vs ~800 GiB for a
-- per-event index carrying tx_hash.
--
-- REPLAY-SAFE: no counts, no sums — re-derives / ch-rebuild / overlapping
-- backfill windows re-insert identical (contract, ledger) keys and RMT
-- collapses them. This is deliberate: a Summing/counted design would
-- double-count on replay (the migration-0059 comet class).
--
-- OPERATOR CONTRACT (same as ops_by_source): presence + non-empty = the
-- reader TRUSTS it, including per-contract emptiness ("no events") and
-- short history ("only these ledgers"). Therefore: apply this DDL (the MV
-- covers everything ingested from that moment), then IMMEDIATELY run the
-- windowed historical backfill to genesis:
--
--   /usr/local/sbin/run-heavy-job.sh contract-ledgers-backfill \
--     /usr/local/bin/stellarindex-ops ch-contract-ledgers-backfill \
--     -ch-addr 127.0.0.1:9300 -from 2 -window 5000000
--
-- Do NOT leave the table applied-but-unbackfilled on a lake with history:
-- quiet contracts would serve truncated event lists stamped as complete.
-- The reader's probe (requireRows) refuses an entirely-empty table, but it
-- cannot see partial coverage.

CREATE TABLE IF NOT EXISTS stellar.contract_active_ledgers
(
    contract_id String,
    ledger_seq  UInt32,
    close_time  DateTime('UTC'),
    ingested_at DateTime DEFAULT now()
)
ENGINE = ReplacingMergeTree(ingested_at)
ORDER BY (contract_id, ledger_seq);

-- SELECT DISTINCT collapses within each insert block (a busy AMM emits many
-- events per contract-ledger); RMT merges collapse the rest across blocks.
CREATE MATERIALIZED VIEW IF NOT EXISTS stellar.contract_active_ledgers_mv
TO stellar.contract_active_ledgers AS
SELECT DISTINCT contract_id, ledger_seq, close_time
FROM stellar.contract_events;
