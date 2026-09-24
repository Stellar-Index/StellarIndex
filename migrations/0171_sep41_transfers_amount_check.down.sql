-- 0171 down — drop the sep41_transfers amount CHECK. Loosening only; no
-- row changes.
BEGIN;

ALTER TABLE sep41_transfers DROP CONSTRAINT IF EXISTS sep41_transfers_amount_check;

COMMIT;
