-- 0004 rollback: restore the ledger > 0 constraint.
--
-- Decompress existing chunks so the constraint swap is allowed
-- (Timescale blocks DROP/ADD CONSTRAINT on a compressed hypertable),
-- then restore the 0001 compression settings (GH-1162). Naming "the
-- compression policy" as the recovery was wrong: add_compression_policy
-- (0001) only SCHEDULES a job against a table with timescaledb.compress
-- enabled, and this down just set that to false — the job fails on every
-- run and the hypertable stays decompressed forever. Restoring it here,
-- same settings as the up's own tail, makes the down's terminal state
-- match the pre-0004 (0001) state instead of a permanently decompressed
-- one.
--
-- The stricter constraint is added with NOT VALID so existing rows
-- are not re-validated; operators explicitly VALIDATE when they're
-- confident no off-chain rows remain:
--
--     ALTER TABLE trades VALIDATE CONSTRAINT trades_ledger_check;

SELECT decompress_chunk(c, true)
FROM show_chunks('trades') c;

ALTER TABLE trades SET (timescaledb.compress = false);

ALTER TABLE trades DROP CONSTRAINT IF EXISTS trades_ledger_check;

ALTER TABLE trades ADD CONSTRAINT trades_ledger_check
    CHECK (ledger > 0) NOT VALID;

ALTER TABLE trades SET (
    timescaledb.compress,
    timescaledb.compress_segmentby = 'base_asset, quote_asset, source',
    timescaledb.compress_orderby   = 'ts DESC, ledger DESC'
);
