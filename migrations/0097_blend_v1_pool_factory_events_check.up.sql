-- 0097 up — admit the V1 pool-factory's 3 extra event kinds into
-- blend_emissions.event_kind / blend_admin.event_kind.
--
-- ROADMAP #89 residual (2026-07-10): a read-only ClickHouse-lake topic
-- census scoped to the 27 gated Blend pools + 2 factories found 778
-- real events across 3 topics that classifyAny (decode_money_market.go)
-- did not classify — the V1 pool-factory's (CCZD6ESM…) own, simpler
-- vocabulary, distinct from the V2 pool events (pool/src/events.rs)
-- already decoded. Real-lake-bytes verified at ledgers 51,524,668 /
-- 51,611,821 / 54,890,906 (see internal/sources/blend/README.md
-- "Known gap" + decode_money_market.go's decoder doc comments):
--
--   update_emissions            (543 events) — 1 topic [Symbol], body
--                                bare i128 — a pool-wide emissions
--                                total, a different concept from V2's
--                                per-reserve reserve_emission_update.
--                                -> blend_emissions.
--   new_liquidation_auction     (234 events) — 2 topics [Symbol,
--                                Address(user)], body Map{bid, lot,
--                                block} — the SAME AuctionData shape
--                                decodeAuctionData already parses for
--                                V2, but with NO auction_type topic and
--                                NO percent field.
--   delete_liquidation_auction  (1 event)   — 2 topics [Symbol,
--                                Address(user)], body ScvVoid.
--
-- The two liquidation-auction kinds land in blend_admin, NOT
-- blend_auctions: blend_auctions.auction_type is NOT NULL with a CHECK
-- BETWEEN 0 AND 2 (UserLiquidation/BadDebt/Interest), and the V1 body
-- carries no auction_type topic to classify against that taxonomy —
-- inventing one would attach unverified provenance to a table whose
-- whole purpose is verified per-protocol data. blend_admin already
-- models heterogeneous per-kind extras via its jsonb attributes column
-- (queue_set_reserve's ReserveConfig does the same), so Target=user +
-- attributes={bid, lot, block} rides there instead — no new columns,
-- CHECK-constraint-only change per the "reuse existing columns"
-- preference.
--
-- Same pattern as 0092/0094/0095: DROP + re-ADD each CHECK with the
-- full set. Additive + old-binary-safe per rule 9 — the previous
-- binary never writes these event_kind values, so widening what's
-- ALLOWED doesn't change its behavior; only the new binary emits rows
-- using the new values. No decompress_chunk needed — 0092/0094/0095
-- established that a CHECK-constraint-only ALTER doesn't require it on
-- this TimescaleDB version (only PK changes do, per 0053/0054/0058/0060).
BEGIN;

ALTER TABLE blend_emissions DROP CONSTRAINT blend_emissions_event_kind_check;
ALTER TABLE blend_emissions ADD CONSTRAINT blend_emissions_event_kind_check CHECK (event_kind IN (
    'gulp', 'claim',
    'reserve_emission_update', 'gulp_emissions',
    'bad_debt', 'defaulted_debt',
    'update_emissions'
));

ALTER TABLE blend_admin DROP CONSTRAINT blend_admin_event_kind_check;
ALTER TABLE blend_admin ADD CONSTRAINT blend_admin_event_kind_check CHECK (event_kind IN (
    'set_admin', 'update_pool',
    'queue_set_reserve', 'cancel_set_reserve', 'set_reserve',
    'set_status', 'deploy',
    'new_liquidation_auction', 'delete_liquidation_auction'
));

COMMIT;
