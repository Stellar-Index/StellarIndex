-- 0087 down — drop the asset_volume_24h rollup.
--
-- Correctness-safe: with the table absent, the /v1/assets listing's
-- per_asset_24h_vol CTE returns no rows and every asset's
-- volume_24h_usd renders as absent (LEFT JOIN → NULL → omitempty). The
-- pre-0087 live per-request SUM is not auto-restored (the CTE now reads
-- the rollup), so a full rollback also needs the code reverted; on its
-- own this only blanks the listing's 24h-volume column.
DROP TABLE IF EXISTS asset_volume_24h;
