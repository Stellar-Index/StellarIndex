-- Revert 0226: drop blnt_events.tx_index.
ALTER TABLE blnt_events DROP COLUMN IF EXISTS tx_index;
