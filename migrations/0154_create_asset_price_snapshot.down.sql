-- 0154 down — drop the asset_price_snapshot rollup.
--
-- Correctness-safe but NOT behaviour-neutral on its own, and in the
-- same way 0087's down is not: with the table absent the /v1/assets
-- listing's LEFT JOIN finds nothing, so every row renders `price_usd`
-- absent, no `change_*_pct`, no `source_count`, and rank tier 1
-- (unpriced) under the volume ordering — i.e. the listing degrades to
-- exactly the shape it shows today for an asset nobody can price. It
-- does NOT serve wrong numbers.
--
-- The pre-0154 inline twelve-CTE derivation is not auto-restored (the
-- listing SELECT no longer contains it), so a full rollback also needs
-- the code reverted — `git revert` the #331 F1 commit, which restores
-- listAssetsBaseSelect's price CTEs and the /*PUSHDOWN_*/ machinery
-- together. Order matters only in one direction: revert the code first
-- if you want the listing priced throughout, since the reverted code
-- does not read this table.

DROP TABLE IF EXISTS asset_price_snapshot;
