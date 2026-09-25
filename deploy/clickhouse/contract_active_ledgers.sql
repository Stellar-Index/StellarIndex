-- si-apply-scope: operator
--
-- NOT applied by any bootstrap. An operator runs this against an EXISTING
-- deployment (r1) for the DDL below plus the backfill runbook it carries. A
-- FRESH host gets these objects from deploy/clickhouse/tier1_schema.sql,
-- which declares each of them identically — the gate named below pins that,
-- so the DDL here is a mirror and the runbook is the reason the file exists.
-- Scope markers are enforced by scripts/ci/lint-ch-apply-scope.sh; the
-- fresh-host apply set is declared in
-- configs/ansible/roles/archival-node/tasks/08-clickhouse.yml.
--
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
-- OPERATOR CONTRACT: table-level presence + non-empty gates the fast path
-- on/off (the reader's requireRows probe refuses an entirely-empty table —
-- MV dropped/TRUNCATEd reads as "index unavailable"). PER-CONTRACT
-- emptiness is deliberately NOT trusted as authoritative "no events":
-- ExplorerReader.ContractEventsRecent (internal/storage/clickhouse/
-- explorer_reader.go, audit W1-chrollup-3) treats an empty per-contract
-- walk as "unknown, fall back" and re-reads contract_events directly,
-- exactly as it did before this index existed — because the table-global
-- probe cannot see PARTIAL backfill coverage. Apply this DDL (the MV
-- covers everything ingested from that moment), then run the windowed
-- historical backfill to genesis:
--
--   /usr/local/sbin/run-heavy-job.sh contract-ledgers-backfill \
--     /usr/local/bin/stellarindex-ops ch-contract-ledgers-backfill \
--     -ch-addr 127.0.0.1:9300 -from 2 -window 5000000 -write
--
-- Leaving the table applied-but-unbackfilled on a lake with history costs
-- PERFORMANCE, not correctness: every quiet/cold contract keeps paying the
-- pre-index scan until the backfill catches it, but the fallback means it
-- never serves a truncated event list stamped as complete.
--
-- ── Step 3: verify ──────────────────────────────────────────────────────
-- Spot-check N contracts: the index's active-ledger set for a contract
-- must equal the distinct ledger set from a direct scan of
-- contract_events for that same contract (bounded per-contract reads):
--
--   SELECT countIf(a != b) FROM (
--     SELECT
--       (SELECT countDistinct(ledger_seq) FROM stellar.contract_events
--         WHERE contract_id = c.contract_id) AS a,
--       (SELECT countDistinct(ledger_seq) FROM stellar.contract_active_ledgers
--         WHERE contract_id = c.contract_id) AS b,
--       c.contract_id
--     FROM (SELECT DISTINCT contract_id FROM stellar.contract_events
--           WHERE ledger_seq > (SELECT max(ledger_seq) - 10000 FROM stellar.ledgers)
--           LIMIT 20) c)
--
-- Expect 0.

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
