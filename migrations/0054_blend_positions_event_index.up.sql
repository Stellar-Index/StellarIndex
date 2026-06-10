-- 0054: blend_positions — switch the per-event discriminator from
-- (asset, user_address) to event_index.
--
-- Migration 0053 added (asset, user_address) to the blend_positions PK to
-- disambiguate multiple position changes in one operation. That handles the
-- common case, but NOT when one operation emits two events of the same
-- (event_kind, asset, user) — those still collide and one is dropped. The
-- completeness verifier showed a persistent Δ≈1118 that re-derivation couldn't
-- close (0 duplicate-(asset,user) rows in the table, yet the decoder emits more
-- distinct events than land). event_index (the contract event's index within
-- the tx) is the only fully-unique discriminator — the same fix used for
-- blend_emissions / blend_admin in 0053. (asset, user_address) stay as columns
-- but leave the PK.
--
-- Existing rows have event_index=0 (column default), so the operator DELETEs
-- blend_positions and re-derives it from ClickHouse with the new sink so every
-- row carries its true event_index.
--
-- Decompress first (TSDB blocks constraint changes on compressed chunks).

SELECT decompress_chunk(c, true) FROM show_chunks('blend_positions') c;

ALTER TABLE blend_positions ADD COLUMN IF NOT EXISTS event_index integer NOT NULL DEFAULT 0;
ALTER TABLE blend_positions DROP CONSTRAINT blend_positions_pkey;
ALTER TABLE blend_positions ADD CONSTRAINT blend_positions_pkey
    PRIMARY KEY (pool, ledger, tx_hash, op_index, event_kind, event_index, ledger_close_time);
