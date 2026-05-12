-- Restore the two GIN indexes dropped in the up migration.
-- See 0029_drop_unused_blend_jsonb_gin_indexes.up.sql for the
-- rationale for the original drop.

CREATE INDEX IF NOT EXISTS blend_auctions_bid_gin
    ON blend_auctions USING gin (bid jsonb_path_ops);

CREATE INDEX IF NOT EXISTS blend_auctions_lot_gin
    ON blend_auctions USING gin (lot jsonb_path_ops);
