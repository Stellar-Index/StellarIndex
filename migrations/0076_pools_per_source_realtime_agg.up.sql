-- 0076 up — enable real-time aggregation on pools_per_source_1h.
--
-- Migration 0036 created the CAGG with TimescaleDB's default
-- materialized_only=true. With that, a query only sees buckets the
-- refresh policy has already materialized — and an hourly bucket
-- isn't materialized until the hour CLOSES (+ the 5-min end_offset).
-- So the in-progress hour was invisible: /v1/pools read zero for the
-- current hour until ~5 min past the top of the next hour. Identical
-- symptom + fix to source_volume_1h (migration 0069, 2026-06-19).
--
-- materialized_only=false turns on real-time aggregation: a query
-- UNIONs the materialized history with a live aggregate of the raw
-- trades ABOVE the materialization watermark (only the current
-- partial hour — bounded + cheap, NOT a full-window rescan). The
-- /v1/pools handler's post-CAGG XLM/USD volume recomputation and
-- 24-bucket last() roll-up work identically on the live-computed
-- rows (same columns).

BEGIN;

ALTER MATERIALIZED VIEW pools_per_source_1h SET (timescaledb.materialized_only = false);

COMMIT;
