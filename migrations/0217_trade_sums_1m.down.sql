-- 0217 down — the view is derived from `trades` and read only by the
-- aggregator, which falls back to a truncated window without it.
DROP MATERIALIZED VIEW IF EXISTS trade_sums_1m;
