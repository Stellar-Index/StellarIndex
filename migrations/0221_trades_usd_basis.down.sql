-- 0221 down — drop `trades.usd_basis` and the restamp log's basis columns.
-- Catalog-only, like the up. The basis is lost; a restamp or re-derive
-- writes it again after re-applying the up.

BEGIN;

ALTER TABLE usd_volume_restamp_log
    DROP COLUMN IF EXISTS usd_basis,
    DROP COLUMN IF EXISTS prior_usd_basis;

ALTER TABLE trades DROP COLUMN IF EXISTS usd_basis;

COMMIT;
