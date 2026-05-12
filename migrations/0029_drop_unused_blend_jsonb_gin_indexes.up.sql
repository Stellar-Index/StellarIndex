-- Drop unused GIN indexes on blend_auctions.bid / .lot.
--
-- The original migration 0009 created two single-column GIN
-- indexes intended to support "did this asset ever get
-- liquidated against?" queries against the JSONB bid/lot
-- columns. No reader in internal/storage/timescale/ ever
-- queries those JSONB structures by content; the two listing
-- queries (LatestBlendAuctionEvent, ListBlendPools) only filter
-- by pool / auction_type / user_address / ts. These indexes
-- impose continuous write-amplification on every blend-auction
-- INSERT for read paths that don't exist.
--
-- F-1238 (audit-2026-05-12). Index-only drop, no schema change;
-- safe to apply concurrently in production. Rollback re-creates
-- the indexes if the asset-centric query path materialises in
-- a future iteration.
--
-- DROP INDEX without CONCURRENTLY because golang-migrate runs
-- inside a transaction and CONCURRENTLY can't be used inside a
-- txn. The blend_auctions table sees one INSERT per swap (low
-- volume, sparse hot path); the brief AccessExclusive lock on
-- the index isn't customer-visible.

DROP INDEX IF EXISTS blend_auctions_bid_gin;
DROP INDEX IF EXISTS blend_auctions_lot_gin;
