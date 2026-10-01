-- 0196 down — drop `trades.tx_index`. Catalog-only, like the up; the
-- tagged values are re-derivable from the lake by tag-tx-index.

BEGIN;

ALTER TABLE trades DROP COLUMN IF EXISTS tx_index;

COMMIT;
