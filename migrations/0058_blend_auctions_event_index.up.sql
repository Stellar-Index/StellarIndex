-- 0058: blend_auctions — add event_index to the PK (F-1324).
--
-- blend_auctions was skipped by the 0053 Blend coarse-PK fix (which covered
-- blend_positions / blend_emissions / blend_admin). Its PK is
-- (ledger, tx_hash, op_index, ts) — no per-event discriminator. A single Blend
-- pool operation can emit MORE THAN ONE auction event of the same kind: a
-- liquidation that fills several positions in one call emits multiple
-- fill_auction events, and a batch admin op can fold a new + a delete onto the
-- same op. They share (ledger, tx_hash, op_index) and the same ts (one ledger
-- close), so the coarse PK collapsed them to one row via ON CONFLICT DO NOTHING
-- — the same coarse-PK data-loss class fixed for blend_positions/emissions/admin
-- (0053/0054), defindex (0055), and soroswap-router (0056). event_index (the
-- contract event's index within its tx — events.Event.EventIndex) is the unique
-- discriminator; event_kind also drags in so a new + a fill at the same
-- event_index slot of a future WASM can't collide.
--
-- Existing rows get event_index=0 (column default). Blend auctions are low
-- volume (a handful per day protocol-wide); on a populated host DELETE +
-- re-derive carries the true event_index (operator runbook), but the table is
-- small enough that the ADD-PK below succeeds directly on existing one-per-op
-- rows. STOP the indexer before this migration and deploy the event_index sink
-- before restarting so the new 6-col ON CONFLICT target matches the new PK.
--
-- TimescaleDB blocks constraint changes on compressed chunks; decompress first.

SELECT decompress_chunk(c, true) FROM show_chunks('blend_auctions') c;

ALTER TABLE blend_auctions ADD COLUMN IF NOT EXISTS event_index integer NOT NULL DEFAULT 0;
ALTER TABLE blend_auctions DROP CONSTRAINT blend_auctions_pkey;
ALTER TABLE blend_auctions ADD CONSTRAINT blend_auctions_pkey
    PRIMARY KEY (ledger, tx_hash, op_index, ts, event_kind, event_index);
