-- 0150 down — drop trades.signer + its index.
--
-- The trades hypertable's existing data is unaffected; only the signer
-- column (and its partial index) is removed.

BEGIN;

DROP INDEX IF EXISTS trades_signer_idx;
ALTER TABLE trades DROP COLUMN IF EXISTS signer;

COMMIT;
