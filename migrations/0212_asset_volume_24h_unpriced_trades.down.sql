-- 0212 down — drop asset_volume_24h.unpriced_trades.
--
-- Pair with a rollback to a pre-0212 binary: the current refresh writes the
-- column and the listing reads it. Nothing is lost; the next pass of a
-- 0212 binary refills it.

BEGIN;

ALTER TABLE asset_volume_24h
    DROP COLUMN IF EXISTS unpriced_trades;

COMMIT;
