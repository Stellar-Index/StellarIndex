-- 0053: fix the coarse-PK data-loss bug in the Blend money-market tables.
--
-- blend_positions / blend_emissions / blend_admin keyed rows on
-- (…, op_index, event_kind, …) with no PER-EVENT discriminator. When a single
-- operation emits more than one event of the same kind — a money-market action
-- that changes several (asset, user) positions, or multiple emission/admin
-- events in one call — every row after the first collides on the primary key
-- and ON CONFLICT DO NOTHING silently drops it. The completeness verifier
-- surfaced this as a persistent projection delta (positions Δ≈3.6k, emissions
-- Δ≈5.3k) that re-derivation could not close (the rows had nowhere to land).
--
-- Fix:
--   • blend_positions — add (asset, user_address) to the PK. Both are existing
--     NOT NULL columns, so the change is non-destructive: every existing row is
--     already correctly keyed, and a re-derive only ADDs the previously-dropped
--     siblings (no DELETE, no duplicate risk).
--   • blend_emissions / blend_admin — asset/user are nullable (absent for some
--     kinds) and can't sit in a PK, so add event_index (the contract event's
--     index within the tx) and key on it. Existing rows get event_index=0; the
--     operator DELETEs + re-derives these two tables so every row carries its
--     true event_index (see docs / the rollout runbook).
--
-- TimescaleDB blocks constraint changes on compressed chunks, so decompress
-- first; the compression policy recompresses lazily afterward.

SELECT decompress_chunk(c, true) FROM show_chunks('blend_positions') c;
SELECT decompress_chunk(c, true) FROM show_chunks('blend_emissions') c;
SELECT decompress_chunk(c, true) FROM show_chunks('blend_admin') c;

ALTER TABLE blend_positions DROP CONSTRAINT blend_positions_pkey;
ALTER TABLE blend_positions ADD CONSTRAINT blend_positions_pkey
    PRIMARY KEY (pool, ledger, tx_hash, op_index, event_kind, asset, user_address, ledger_close_time);

ALTER TABLE blend_emissions ADD COLUMN IF NOT EXISTS event_index integer NOT NULL DEFAULT 0;
ALTER TABLE blend_emissions DROP CONSTRAINT blend_emissions_pkey;
ALTER TABLE blend_emissions ADD CONSTRAINT blend_emissions_pkey
    PRIMARY KEY (pool, ledger, tx_hash, op_index, event_kind, event_index, ledger_close_time);

ALTER TABLE blend_admin ADD COLUMN IF NOT EXISTS event_index integer NOT NULL DEFAULT 0;
ALTER TABLE blend_admin DROP CONSTRAINT blend_admin_pkey;
ALTER TABLE blend_admin ADD CONSTRAINT blend_admin_pkey
    PRIMARY KEY (contract_id, ledger, tx_hash, op_index, event_kind, event_index, ledger_close_time);
