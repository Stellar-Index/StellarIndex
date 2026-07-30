-- 0123 down — drop the per-account trades indexes.

DROP INDEX IF EXISTS trades_taker_ts_idx;
DROP INDEX IF EXISTS trades_maker_ts_idx;
