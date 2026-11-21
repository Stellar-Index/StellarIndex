-- 0235 down — drop the trades amount-legs CHECK. Catalog-only, loosening;
-- compressed chunks stay compressed.

BEGIN;

ALTER TABLE trades DROP CONSTRAINT IF EXISTS trades_amount_legs_check;

COMMIT;
