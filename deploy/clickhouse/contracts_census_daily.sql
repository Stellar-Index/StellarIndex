-- contracts_census_daily — day-keyed per-contract event counts behind
-- the /v1/contracts directory (open-fixes inventory #26 item 2).
--
-- WHY: the directory's census query (uniqExact over the PK of
-- billions-row contract_events, GROUP BY contract_id over multi-day
-- windows) measured 40s / 790M rows per run and ran ~160 times per 3h
-- across the prewarm rungs. Ranking from contract_events_daily was
-- MEASURED WORSE (24.5s — merging ~15M uniqCombined HLL states), and
-- contract_active_ledgers is contract-first so a window scan doesn't
-- prune. The durable shape is plain per-day counts computed ONCE per
-- day.
--
-- DUP-SAFETY (the migration-0059 Summing-MV double-count class): this
-- table has NO MV. A periodic job (stellarindex-ops ch-census-rollup)
-- recomputes whole days and swaps them in atomically with
-- ALTER TABLE … REPLACE PARTITION — recomputing a day is idempotent by
-- construction, and a partial write never mixes with served rows.
-- Completed days are written once; the current (partial) day is
-- re-replaced every cycle, so the serving tail is at most one cadence
-- (30 min) stale — inside the directory's SWR staleness budget.
--
-- OPERATOR CONTRACT (same as the sibling indexes): presence +
-- non-empty = the reader TRUSTS it for any window whose floor is
-- covered. Apply the DDL, then run the historical fill promptly:
--
--   /usr/local/sbin/run-heavy-job.sh census-rollup-backfill \
--     /usr/local/bin/stellarindex-ops ch-census-rollup \
--     -ch-addr 127.0.0.1:9300 -backfill
--
-- and install the 30-min timer for the incremental cycle.

CREATE TABLE IF NOT EXISTS stellar.contracts_census_daily
(
    day         Date,
    contract_id String,
    -- uniqExact over (ledger_seq, tx_hash, op_index, event_index) —
    -- exact, matching the legacy census (audit: count() over-counted
    -- via lake duplicate rows).
    events      UInt64,
    last_ledger UInt32,
    last_seen   DateTime('UTC')
)
ENGINE = MergeTree
PARTITION BY day
ORDER BY (day, contract_id);

CREATE TABLE IF NOT EXISTS stellar.contracts_census_daily_staging
AS stellar.contracts_census_daily;
