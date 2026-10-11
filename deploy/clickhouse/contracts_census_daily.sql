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
-- contracts_census_daily — day-keyed per-contract event counts behind
-- the /v1/contracts directory.
--
-- WHY: a census over billions-row contract_events (uniqExact, GROUP BY
-- contract_id over multi-day windows) cost ~40s per run. Ranking from
-- contract_events_daily is worse (merging uniqCombined states), and
-- contract_active_ledgers is contract-first so a window scan doesn't
-- prune. Plain per-day counts computed once per day avoid both.
--
-- DUP-SAFETY (the migration-0059 Summing-MV double-count class): no MV.
-- stellarindex-ops ch-census-rollup recomputes whole days and swaps them in
-- with ALTER TABLE … REPLACE PARTITION, so a recompute is idempotent and a
-- partial write never mixes with served rows. The current day is re-replaced
-- every cycle, so the tail is at most one cadence (30 min) stale.
--
-- OPERATOR CONTRACT (same as the sibling indexes): presence + non-empty =
-- the reader TRUSTS it for any window whose floor is covered. Apply the
-- DDL, then run the historical fill promptly:
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
-- uniqExact over (ledger_seq, tx_hash, op_index, event_index): count()
-- over-counts lake duplicate rows.
    events      UInt64,
    last_ledger UInt32,
    last_seen   DateTime('UTC')
)
ENGINE = MergeTree
-- Day, not intDiv(ledger, 1000000) like the lake tables: the day IS the unit
-- ch-census-rollup recomputes and swaps with REPLACE PARTITION '<day>'.
PARTITION BY day
ORDER BY (day, contract_id);

-- Each ch-census-rollup run computes into a private
-- stellar.contracts_census_daily_staging_<16 hex> it creates and drops, so no
-- shared staging table exists. This drop clears an empty orphan on hosts
-- provisioned when one was still declared; re-run this file there only for it.
DROP TABLE IF EXISTS stellar.contracts_census_daily_staging;
