-- 0123 up — account-leading indexes for GET /v1/accounts/{g}/trades.
--
-- The per-address trades endpoint reads
--
--   SELECT … FROM trades
--    WHERE taker = $1                       -- (and a mirrored maker arm)
--      AND (ts, ledger, tx_hash, op_index) < ($2, $3, $4, $5)
--    ORDER BY ts DESC, ledger DESC, tx_hash DESC, op_index DESC
--    LIMIT n
--
-- (internal/storage/timescale/account_trades.go). `trades` carries NO
-- account-leading index — every existing index leads with asset, pair,
-- or source (0001/0025/0037) — so without these two the read is a
-- full hypertable scan, the exact class CLAUDE.md's "No unbounded
-- trade-scan queries" rule exists for.
--
-- WHY taker/maker, NOT a source_account column: `trades` has never
-- carried a source_account column (verified against both this
-- migrations directory and the live r1 schema, 2026-07-30). The
-- account attribution the table DOES hold is `taker` (the acting
-- account — the op/tx source for sdex, the user address for
-- aquarius/phoenix/comet) and `maker` (the resting-offer account,
-- sdex only). Both are NULL for off-chain CEX/FX rows (no Stellar
-- account exists) — hence the partial predicates: the indexes only
-- carry rows that can ever match a per-account lookup. (soroswap rows
-- were also NULL until fda0aea0 taught its decoder the SwapEvent `to`
-- field; the historical replay retro-fills them via the upsert.)
--
-- The index column list mirrors the query's full ORDER BY (including
-- the tx_hash tie-breaker — two same-account trades can share
-- (ts, ledger, op_index) across two txs in one ledger) so the planner
-- walks each arm in output order and stops at LIMIT, no sort node.
--
-- ── APPLYING TO AN ALREADY-POPULATED DATABASE (e.g. r1) ──
-- Same drill as migration 0037: `trades` holds ~548M rows and a plain
-- in-transaction CREATE INDEX write-locks the table for the whole
-- build, so ON A LIVE NODE build both by hand FIRST. NOTE: hypertables
-- REJECT `CREATE INDEX CONCURRENTLY` ("hypertables do not support
-- concurrent index creation" — hit on r1 2026-07-30); the Timescale
-- equivalent is per-chunk transactions, which lock one chunk at a time:
--
--   CREATE INDEX trades_taker_ts_idx
--       ON trades (taker, ts DESC, ledger DESC, tx_hash DESC, op_index DESC)
--       WITH (timescaledb.transaction_per_chunk)
--       WHERE taker IS NOT NULL;
--   CREATE INDEX trades_maker_ts_idx
--       ON trades (maker, ts DESC, ledger DESC, tx_hash DESC, op_index DESC)
--       WITH (timescaledb.transaction_per_chunk)
--       WHERE maker IS NOT NULL;
--
-- then run `stellarindex-migrate up` — IF NOT EXISTS makes the
-- statements below no-ops. Note additionally that 247/249 trades
-- chunks are COMPRESSED on r1 (compression segmentby is
-- (base_asset, quote_asset, source) — no account column), so these
-- indexes accelerate the uncompressed recent window only; deep
-- history still decompresses per-chunk and the endpoint's 8s read
-- budget + 503-timeout contract is the backstop. The serving code
-- does not assume the index exists (it only gets slower without it).

CREATE INDEX IF NOT EXISTS trades_taker_ts_idx
    ON trades (taker, ts DESC, ledger DESC, tx_hash DESC, op_index DESC)
    WHERE taker IS NOT NULL;

CREATE INDEX IF NOT EXISTS trades_maker_ts_idx
    ON trades (maker, ts DESC, ledger DESC, tx_hash DESC, op_index DESC)
    WHERE maker IS NOT NULL;
