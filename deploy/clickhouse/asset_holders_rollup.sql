-- asset_holders_rollup — precomputed per-asset holders boards
-- (inventory #4, open-fixes-inventory-2026-08-08).
--
-- WHY: GET /v1/assets/{id}/holders ran TWO ledger_entries_current FINAL
-- scans (ranking + count) PER REQUEST — 8-15s each under load, an
-- effectively-100% failure rate for random assets in the 2026-08-07
-- audit, and squarely outside the sub-second page budget (operator goal
-- 2026-08-08).
--
-- SHAPE: `stellarindex-ops ch-holders-rollup` (30-min timer) recomputes
-- every asset's top-500 board + holder count into the *_staging twins
-- with the SAME two FINAL scans — once per cycle instead of per request
-- — then atomically EXCHANGE TABLES them live. Readers do keyed
-- sub-millisecond lookups; an asset with no row after a completed cycle
-- authoritatively has no positive-balance holders. Plain MergeTree (not
-- Replacing): the exchange replaces the WHOLE table every cycle, so
-- there is no merge/dup story at all.
--
-- The reader's requireRows probe refuses an entirely-empty live table
-- (never-exchanged staging pathology) and falls back to the legacy
-- per-request scans — slow but correct.

CREATE TABLE IF NOT EXISTS stellar.asset_holders_rollup
(
    asset       String,
    rank        UInt32,
    account_id  String,
    balance     Int64,
    computed_at DateTime DEFAULT now()
)
ENGINE = MergeTree
ORDER BY (asset, rank);

CREATE TABLE IF NOT EXISTS stellar.asset_holders_rollup_staging
AS stellar.asset_holders_rollup;

CREATE TABLE IF NOT EXISTS stellar.asset_holders_counts
(
    asset       String,
    holders     Int64,
    computed_at DateTime DEFAULT now()
)
ENGINE = MergeTree
ORDER BY asset;

CREATE TABLE IF NOT EXISTS stellar.asset_holders_counts_staging
AS stellar.asset_holders_counts;
