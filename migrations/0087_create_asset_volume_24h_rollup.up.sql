-- 0087 up — `asset_volume_24h` worker-maintained rollup.
--
-- Backs the `volume_24h_usd` column on the GET /v1/assets listing
-- (internal/storage/timescale/coins.go's `per_asset_24h_vol` CTE). The
-- live derivation is a per-request SUM(volume_usd) over the prices_1m
-- continuous aggregate, single-sided per asset (base OR quote) via a
-- UNION ALL then GROUP BY asset_id. On the UNFILTERED listing that
-- materialises stats for every asset (~256k rows / ~1.3M buffer hits
-- for a one-page result) — measured ~4.8s cold during the 2026-07-06
-- latency incident.
--
-- It cannot be a TimescaleDB continuous aggregate: the single-sided
-- per-asset figure needs a UNION of the base_asset and quote_asset
-- projections of the same source, which a CAGG's single GROUP BY can't
-- express. So an aggregator worker
-- (internal/aggregate/assetvolrollup) runs the exact base-OR-quote SUM
-- on a slow cadence and upserts one row per asset here. The listing
-- then LEFT JOINs a small keyed-on-PK table instead of re-summing
-- prices_1m per request.
--
-- Schema (one row per asset with trailing-24h volume):
--   asset_id    — canonical asset id (base OR quote side), e.g.
--                 'USDC-G…', 'native'.
--   vol_usd     — SUM(prices_1m.volume_usd) over the trailing 24h where
--                 the asset was base OR quote. NUMERIC (ADR-0003) so the
--                 listing renders the identical decimal string the live
--                 SUM produced — the rollup only moves the compute, not
--                 the value/scale.
--   computed_at — worker run timestamp; the worker prunes rows it did
--                 not re-write this pass (assets whose 24h volume
--                 lapsed to nothing).
--
-- Not a hypertable: keyed on asset (bounded by the active-volume asset
-- set), UPDATE'd in place. Reads return NULL volume until the
-- aggregator worker's first pass populates it (LEFT JOIN → COALESCE 0,
-- same posture as change_summary_5m).

BEGIN;

CREATE TABLE asset_volume_24h (
    asset_id    text        PRIMARY KEY,
    vol_usd     numeric     NOT NULL,
    computed_at timestamptz NOT NULL DEFAULT now()
);

COMMENT ON TABLE asset_volume_24h IS
    'Worker-maintained trailing-24h per-asset USD trade-volume rollup '
    'backing /v1/assets volume_24h_usd (migration 0087, #43). Refreshed '
    'by internal/aggregate/assetvolrollup in the aggregator binary.';

COMMIT;
