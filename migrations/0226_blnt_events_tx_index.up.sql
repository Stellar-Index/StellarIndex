-- 0226 up — blnt_events.tx_index, the transaction's apply position in its ledger.
--
-- Invariant: tx_index is the 0-based position of the row's transaction in the ledger's TxProcessing
-- (apply) order, equal to stellar.transactions.tx_index, or NULL only where no apply order was available.
--
-- Nullable with no default, so ADD COLUMN is catalog-only and the previous binary's INSERT, which does
-- not name it, keeps working; projected-rebuild fills existing rows.

BEGIN;

SET LOCAL lock_timeout = '5s';

ALTER TABLE blnt_events ADD COLUMN IF NOT EXISTS tx_index integer;

COMMENT ON COLUMN blnt_events.tx_index IS
    '0-based position of the transaction in the ledger''s apply (TxProcessing) order, as stellar.transactions.tx_index; NULL = unknown';

COMMIT;
