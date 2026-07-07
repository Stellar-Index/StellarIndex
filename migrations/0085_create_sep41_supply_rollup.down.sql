-- 0085 down — drop the SEP-41 supply rollup checkpoint.
--
-- The reader (SEP41KindTotalsAtOrBefore) falls back to the full
-- per-contract aggregate over sep41_supply_events when the rollup is
-- absent, so dropping this table is correctness-safe — it only
-- restores the (slow) pre-0085 read path.
DROP TABLE IF EXISTS sep41_supply_rollup;
