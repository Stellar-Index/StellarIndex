-- 0117 down — drop `sac_wrapped_stroops` from asset_supply_history,
-- reverting the schema half of the E4/N-F3(b) subset-compare plumbing.
--
-- The post-0117 binary names this column in InsertSupply's INSERT
-- column list and in LatestSupply / SupplyHistory's SELECT list, so a
-- rollback of this migration must be paired with a rollback of the
-- binary to the pre-0117 shape. down.sql is a local/dev iteration
-- lever, not a production rollback lever (README rule 9).
--
-- Data note: dropping the column discards the recorded SACWrapped
-- component for every post-0117 snapshot. Nothing else derives from it
-- (total_supply already includes it as an addend and is unaffected),
-- and the aggregator repopulates it on the next refresh cycle if the
-- column is re-added — so the loss is recoverable in one bucket close
-- for CURRENT values, but the historical per-snapshot series is not.
--
-- DROP COLUMN on a compressed hypertable is supported directly on
-- TimescaleDB 2.11+ (r1 runs 2.26) — same as 0111's and 0114's downs.

BEGIN;

ALTER TABLE asset_supply_history
    DROP COLUMN IF EXISTS sac_wrapped_stroops;

COMMIT;
