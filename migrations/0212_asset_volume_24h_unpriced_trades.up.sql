-- 0212 up — count the trades asset_volume_24h.vol_usd could not value.
--
-- vol_usd sums prices_1m.volume_usd, which is sum(coalesce(usd_volume, 0)):
-- a trade with no trade-time usd_volume (stale XLM anchor, token/token with
-- no route, pre-restamp history) adds 0, and the /v1/assets listing served
-- the sum as a total. unpriced_trades is the number of such trades, base OR
-- quote, over the 24h window; the listing serves volume_lower_bound when it
-- is > 0.
--
-- The aggregator's 2-minute assetvolrollup pass fills it for every row it
-- upserts, so it is correct from the first pass after deploy. No backfill.
--
-- Additive (migrations/README.md rule 9): asset_volume_24h is a plain table,
-- NOT NULL DEFAULT 0 is a metadata-only ADD COLUMN, and the previous
-- binary's upsert does not name the column. Executed in
-- test/integration/asset_volume_unpriced_test.go.

BEGIN;

ALTER TABLE asset_volume_24h
    ADD COLUMN IF NOT EXISTS unpriced_trades bigint NOT NULL DEFAULT 0; -- lint-money:ok a trade count, not an amount

COMMENT ON COLUMN asset_volume_24h.unpriced_trades IS
    'Trades in the trailing 24h (asset as base OR quote) with no trade-time '
    'usd_volume, so vol_usd excludes them. > 0 means vol_usd is a lower bound.';

COMMIT;
