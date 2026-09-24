-- 0154 down — drop the asset_price_snapshot rollup.
--
-- Revert the code FIRST; this down is not safe under a binary that reads
-- the table. The /v1/assets listing SQL LEFT JOINs asset_price_snapshot,
-- and a join against a missing relation is a 42P01 (undefined_table) query
-- error, not an empty join: with the table absent the listing query fails
-- instead of degrading to the unpriced shape. ContractCatalogueRows and the
-- refresher (RefreshAssetListingRollups) fail the same way.
--
-- The pre-0154 inline twelve-CTE derivation is not auto-restored (the
-- listing SELECT no longer contains it), so a rollback needs the code
-- reverted — `git revert` the #331 F1 commit, which restores
-- listAssetsBaseSelect's price CTEs and the /*PUSHDOWN_*/ machinery
-- together. The reverted code does not read this table, so deploy it
-- first and run this down after.

DROP TABLE IF EXISTS asset_price_snapshot;
