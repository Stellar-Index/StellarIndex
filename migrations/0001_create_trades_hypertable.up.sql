-- 0001 up — core `trades` hypertable + retention policy.
--
-- Every ingested trade (from SDEX, Soroswap, Aquarius, Phoenix, Comet,
-- CEX venues, …) lands in exactly one row here. Identity is
-- (source, ledger, tx_hash, op_index) — matches canonical.Trade.ID().
--
-- Amounts are NUMERIC (arbitrary precision) because Soroban i128
-- token quantities exceed int64. ADR-0003.
--
-- Asset identifiers are stored as text in canonical wire form
-- ("native", "USDC-G…", "C…"). internal/canonical/asset.go
-- produces these via Asset.Value() / accepts via Asset.Scan().

BEGIN;

-- Extension: TimescaleDB (idempotent; required for hypertable + CAGGs).
CREATE EXTENSION IF NOT EXISTS timescaledb;

-- Primary trade table.
CREATE TABLE trades (
    source          text         NOT NULL,
    ledger          integer      NOT NULL CHECK (ledger > 0),
    tx_hash         char(64)     NOT NULL,
    op_index        integer      NOT NULL CHECK (op_index >= 0),

    ts              timestamptz  NOT NULL,

    base_asset      text         NOT NULL,
    quote_asset     text         NOT NULL,
    base_amount     numeric      NOT NULL CHECK (base_amount > 0),
    quote_amount    numeric      NOT NULL CHECK (quote_amount > 0),

    -- USD-denominated volume computed at ingest time using the
    -- quote-asset USD price at ts. NULL during the brief window
    -- between trade insert and the aggregator backfilling the
    -- usd_volume column. API responses coalesce NULLs to "unknown".
    usd_volume      numeric,

    maker           text,
    taker           text,

    -- Book-keeping; useful for debugging but not part of identity.
    ingested_at     timestamptz  NOT NULL DEFAULT now(),

    PRIMARY KEY (source, ledger, tx_hash, op_index, ts)
);

COMMENT ON TABLE trades IS
    'All observed trades, one row per (source, ledger, tx_hash, op_index). '
    'Hypertable partitioned on ts. See ADR-0006.';

COMMENT ON COLUMN trades.base_asset IS
    'Canonical asset identifier — native | <code>-<issuer> | <contract_id>.';
COMMENT ON COLUMN trades.usd_volume IS
    'Derived by the aggregator post-insert; null until that run completes.';

-- Promote to hypertable. Chunk interval 1 day — small enough that
-- compression + retention policies get good granularity; large enough
-- that chunk management overhead stays reasonable at ~150 trade/s
-- network-wide rate.
SELECT create_hypertable(
    'trades',
    'ts',
    chunk_time_interval => INTERVAL '1 day',
    if_not_exists       => TRUE
);

-- Secondary indexes for common API query shapes.
-- 1. Asset-centric lookups (Freighter "asset detail for X").
CREATE INDEX trades_base_ts_idx  ON trades (base_asset,  ts DESC);
CREATE INDEX trades_quote_ts_idx ON trades (quote_asset, ts DESC);

-- 2. Pair-centric lookups (`/v1/pairs?base=X&quote=Y`).
CREATE INDEX trades_pair_ts_idx ON trades (base_asset, quote_asset, ts DESC);

-- 3. Source-centric lookups (cursor replay, debugging).
CREATE INDEX trades_source_ledger_idx ON trades (source, ledger DESC);

-- Compression settings. Group chunks by asset pair + source for good
-- column dictionary reuse; order within-chunk by time DESC.
ALTER TABLE trades SET (
    timescaledb.compress,
    timescaledb.compress_segmentby = 'base_asset, quote_asset, source',
    timescaledb.compress_orderby   = 'ts DESC, ledger DESC'
);

-- Compress chunks older than 7 days. This bounds the hot-write
-- "uncompressed" window and delivers ~10× storage reduction per
-- ADR-0006 consequences.
SELECT add_compression_policy('trades', INTERVAL '7 days');

-- Retention of raw grain: drop chunks older than 90 days.
-- Hourly+ aggregates (migration 0002) live forever; the raw detail
-- ages out but the rolled-up series does not.
SELECT add_retention_policy('trades', INTERVAL '90 days');

-- Cursor store — one row per (source, component). The indexer
-- writes its last-committed ledger after each batch so restarts
-- resume from the right place.
CREATE TABLE ingestion_cursors (
    source          text         NOT NULL,
    -- Optional sub-component (e.g. "factory", "pair:CAB...") for
    -- sources that track multiple positions independently.
    sub_source      text         NOT NULL DEFAULT '',

    last_ledger     integer      NOT NULL CHECK (last_ledger >= 0),
    last_updated    timestamptz  NOT NULL DEFAULT now(),

    PRIMARY KEY (source, sub_source)
);

COMMENT ON TABLE ingestion_cursors IS
    'Per-source ingestion cursors for resumable backfill + live replay.';

COMMIT;
