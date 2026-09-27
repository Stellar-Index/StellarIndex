-- 0174 down — drop the sep41_transfers amount CHECK if present. The up is
-- a no-op since v0.92.1, but a database that applied v0.92.0's up has the
-- constraint, so IF EXISTS reverses both bodies. Loosening only; no row
-- changes.
BEGIN;

ALTER TABLE sep41_transfers DROP CONSTRAINT IF EXISTS sep41_transfers_amount_check;

COMMIT;
