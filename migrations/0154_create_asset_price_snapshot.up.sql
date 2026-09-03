-- 0154 up — `asset_price_snapshot` worker-maintained rollup.
--
-- Backs the `price_usd`, `change_1h_pct`, `change_24h_pct`,
-- `change_7d_pct` and `source_count` columns — AND the leading
-- `rank_tier` sort key — on the GET /v1/assets listing (#331 F1). It is
-- the price twin of migration 0087 (`asset_volume_24h`, #43), which
-- moved the same endpoint's volume column off the request path for the
-- same reason.
--
-- The live derivation is twelve `DISTINCT ON … FROM prices_1m` CTEs
-- materialised for EVERY asset, on EVERY uncached listing variant,
-- whatever page was asked for: four USD-proxy-quoted lookbacks
-- (`direct_usd`, `_1h`, `_24h`, `_7d`), four XLM-quoted lookbacks each
-- reading BOTH stored directions via a UNION ALL (`asset_vs_xlm*`), and
-- four scalar XLM/USD lookups (`xlm_usd*`). Measured on r1 2026-09-02:
--
--   pg_stat_statements (since 2026-07-06), unfiltered listing shapes
--     8,019 calls  mean 2,400 ms  max 10,295 ms  380,324 buf hits/call
--     6,312 calls  mean 2,136 ms  max  9,777 ms  379,026 buf hits/call
--     2,679 calls  mean 1,461 ms  max  4,774 ms  400,499 buf hits/call
--     1,492 calls  mean 1,482 ms  max 15,608 ms  759,332 buf hits/call
--     → 10.8 CPU-hours of Postgres to serve ~116 rows a time.
--
--   EXPLAIN (ANALYZE, BUFFERS) `?limit=50`, volume order, warm
--     1,830 ms · 348,442 shared hits · 51 MB `external merge` spill
--     of which the eight DISTINCT ON CTEs were 1,353 ms
--     (`asset_vs_xlm` alone 881 ms and the whole disk sort).
--
-- It cannot be a TimescaleDB continuous aggregate. A CAGG is
-- `time_bucket(...) GROUP BY` over one projection of one source; this
-- substrate is "the LATEST row per asset across two USD-proxy quote
-- forms, two XLM identity forms and both stored directions, over a
-- 7-day lookback, triangulated through a scalar" — a `DISTINCT ON` over
-- a UNION, which a single GROUP BY cannot express. Same wall 0087 and
-- 0149 hit. Nor a materialised view: `REFRESH MATERIALIZED VIEW` takes
-- ACCESS EXCLUSIVE and would stall concurrent reads of the flagship
-- customer-facing endpoint for the length of the recompute.
--
-- So the aggregator's asset-rollup worker runs the identical derivation
-- on a slow cadence (2 min, alongside asset_volume_24h and in the SAME
-- transaction, so a listing row's price and volume are always from one
-- pass) and upserts one row per PRICED asset here (~10.5k on r1, vs a
-- 198k-row catalogue). The listing then LEFT JOINs a small keyed-on-PK
-- table.
--
-- Schema (one row per asset that HAS a servable price):
--   asset_id       — canonical asset id, e.g. 'USDC-G…', 'native',
--                    'C…'. A row EXISTS iff the COALESCE chain produced
--                    a price this pass; an unpriced asset simply has no
--                    row (LEFT JOIN → NULL → rank tier 1), which is the
--                    same shape the inline derivation produced.
--   price_usd      — the headline USD price, UNROUNDED. NUMERIC
--                    (ADR-0003): the listing applies the identical
--                    ROUND(…, 10)::text wire rendering it always
--                    applied, so the served decimal string is
--                    byte-identical to the inline form's. Never float.
--   change_1h_pct  — (latest / lookback − 1) × 100, UNFORMATTED. NUMERIC
--   change_24h_pct   for the same reason: the listing applies the
--   change_7d_pct    identical to_char(…, 'FM999999990.00'). NULL when
--                    either side of the ratio is missing.
--   source_count   — distinct venues backing price_usd (the latest
--                    per-asset bucket's prices_1m.sources) — the
--                    liquidity signal the API's market-cap valuation
--                    guard reads. NULL for native XLM (triangulated,
--                    always liquid).
--   computed_at    — worker run timestamp. Two jobs: the worker prunes
--                    rows it did not re-write this pass (assets whose
--                    price lapsed), and the LISTING'S JOIN CARRIES A
--                    FLOOR ON IT — internal/storage/timescale's
--                    assetPriceSnapshotMaxAge, 15 minutes — so a wedged
--                    or dead aggregator makes the join MISS rather than
--                    serve an indefinitely-old price. Past the ceiling
--                    the row renders exactly as an unpriced asset does.
--                    Fail-closed: the staleness contract is enforced in
--                    SQL, not by convention.
--
-- Staleness contract in full (also in asset_price_snapshot.go):
--   prices_1m CAGG lag ≤ ~90 s + 2 min refresh + 2 min API cache TTL
--   ⇒ worst-case served price age ≈ 5.5 min, was ≈ 3.5 min. That is
--   older than GET /v1/price (last CLOSED bucket, ADR-0015, ~85-94 s
--   observed) — but the listing was already the staler of the two, and
--   this widens the gap by the refresh interval, not without bound.
--   GET /v1/assets/{id} is deliberately NOT moved onto this table and
--   stays the freshest per-asset surface.
--
-- Not a hypertable: keyed on asset (bounded by the priced asset set),
-- UPDATE'd in place. Reads return NULL price until the aggregator
-- worker's first pass populates it — same posture as asset_volume_24h.
-- ⚠ On a FRESH database the /v1/assets listing therefore shows no
-- prices until the aggregator has run once (≤ 2 min after it starts).

BEGIN;

CREATE TABLE asset_price_snapshot (
    asset_id       text        PRIMARY KEY,
    price_usd      numeric     NOT NULL,
    change_1h_pct  numeric,
    change_24h_pct numeric,
    change_7d_pct  numeric,
    source_count   integer,
    computed_at    timestamptz NOT NULL DEFAULT now()
);

COMMENT ON TABLE asset_price_snapshot IS
    'Worker-maintained per-asset headline USD price + 1h/24h/7d change + '
    'backing source count, backing /v1/assets price_usd, change_*_pct, '
    'source_count and rank_tier (migration 0154, #331 F1). Refreshed by '
    'Store.RefreshAssetListingRollups in the aggregator binary, in the '
    'same transaction as asset_volume_24h. A row exists only while the '
    'asset is priceable; the listing additionally refuses rows older '
    'than assetPriceSnapshotMaxAge.';

COMMENT ON COLUMN asset_price_snapshot.price_usd IS
    'Headline USD price, UNROUNDED NUMERIC (ADR-0003). Direct USD-proxy '
    'quote first, else the XLM price triangulated through XLM/USD; '
    'native XLM takes the 24h-bounded xlm_usd scalar ahead of either. '
    'The listing applies ROUND(price_usd, 10)::text on read.';

COMMENT ON COLUMN asset_price_snapshot.computed_at IS
    'Refresh-pass timestamp. Drives the sibling prune AND the listing '
    'join''s staleness floor (assetPriceSnapshotMaxAge, 15 min) — past '
    'it the listing serves no price rather than a stale one.';

COMMIT;
