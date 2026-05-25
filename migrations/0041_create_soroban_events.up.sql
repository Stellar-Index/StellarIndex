-- 0041 up — `soroban_events` raw-event landing zone (ADR-0029).
--
-- Materialises EVERY Soroban contract event the dispatcher sees as
-- a row so that future per-source decoder backfills become SQL
-- queries rather than MinIO re-walks:
--
--   INSERT INTO blend_positions / cctp_events / whatever
--     SELECT ...
--       FROM soroban_events
--      WHERE contract_id IN (...) AND topic_0_sym IN (...)
--
-- — milliseconds-to-minutes instead of hours-per-source. The table
-- is additive — existing per-source hypertables (trades,
-- blend_auctions, cctp_events, rozo_events, sep41_supply_events,
-- ...) continue to be written from the live dispatcher; this is the
-- catch-all that any future decoder can backfill from.
--
-- Volume estimate (memo + ADR-0029): 50M Soroban-era ledgers ×
-- ~5-20 events/ledger ≈ 250M-1B rows. At ~400 bytes compressed =
-- 100GB-400GB. Manageable against the 13.85 TB Postgres pool.
--
-- Schema choices:
--
--   - `contract_id` is stored in both C-strkey form (text) AND raw
--     32-byte form (bytea). The strkey is what humans paste into
--     `WHERE contract_id = 'C...'`; the raw hex is for byte-equality
--     joins and index efficiency when downstream decoders bulk-match
--     against a set of contract IDs.
--   - Topics 0-3 are stored as their raw XDR bytes (`topic_*_xdr`).
--     `topic_0_sym` is the convenience-decoded Symbol/String form of
--     topic[0], NULL when topic[0] is not a Symbol or String (e.g.
--     a SEP-41 Address-topic event from a non-standard layout).
--     Downstream decoders read raw XDR to get full fidelity; the
--     `topic_0_sym` column exists so `WHERE topic_0_sym = 'swap'`
--     index lookups are cheap.
--   - `body_xdr` is the raw XDR-encoded SCVal body of the event.
--     The downstream decoder unmarshals it. Per ADR-0003 the body
--     can contain i128/u128 amounts; storing the raw XDR keeps that
--     precision intact rather than truncating to a typed column.
--   - `op_args_xdr` carries the InvokeContract op's args when the
--     event was produced by an InvokeContract call. This is needed
--     by sources like RedStone (the WritePrices event body carries
--     prices+timestamps but no feed_id — feed_ids live in the op's
--     args) and is the existing `events.Event.OpArgs` plumbing that
--     the dispatcher already populates. Empty when the event came
--     from a non-InvokeContract op (CAP-67 transfer events tied to
--     classic ops, system events, etc.). Stored as the
--     concatenation of base64-encoded XDR blobs is the existing wire
--     format on `events.Event.OpArgs`, but here we store the
--     concatenated raw XDR with a length-prefix delimiter — wait,
--     actually `op_args_xdr` here is the SDK's InvokeContract
--     argument vector as raw XDR (the whole args list, not one
--     per-arg blob). Downstream consumers parse with the standard
--     XDR machinery.
--
-- Index strategy (per ADR-0029): three indexes for the canonical
-- query shapes future decoder backfills will use.
--
--   1. (contract_id, ledger_close_time DESC) — "every event from
--      contract X, newest first" — the dominant decoder-backfill
--      shape ("INSERT ... SELECT FROM soroban_events WHERE
--      contract_id IN (...)").
--   2. (topic_0_sym, ledger_close_time DESC) WHERE topic_0_sym IS
--      NOT NULL — cross-contract symbol scan ("every `swap` event
--      across every contract").
--   3. (contract_id, topic_0_sym) WHERE topic_0_sym IS NOT NULL —
--      the per-decoder "this contract + this symbol" lookup the
--      common case "INSERT ... WHERE contract_id IN (...) AND
--      topic_0_sym IN (...)" walks.
--
-- ── CREATE INDEX vs CREATE INDEX CONCURRENTLY ──
-- Indexes are created on the freshly-created empty table BEFORE any
-- rows exist, so plain CREATE INDEX is fast and lock-free.
-- TimescaleDB rejects CONCURRENTLY on hypertables (each chunk's
-- index is built separately) — the 0037 migration's CONCURRENTLY
-- workaround applies to ALREADY-POPULATED hypertables, not fresh
-- ones. Don't copy that pattern here.
--
-- Compression: segment-by contract_id (a decoder backfill almost
-- always groups by contract), order-by ledger_close_time DESC so
-- per-segment chunks read newest-first.

BEGIN;

CREATE TABLE soroban_events (
    ledger              integer      NOT NULL CHECK (ledger >= 0),
    ledger_close_time   timestamptz  NOT NULL,
    tx_hash             bytea        NOT NULL,   -- 32-byte raw hash
    op_index            smallint     NOT NULL CHECK (op_index >= 0),
    event_index         smallint     NOT NULL CHECK (event_index >= 0),

    -- Emitting contract; both human-readable strkey + raw 32 bytes.
    contract_id         text         NOT NULL,
    contract_id_hex     bytea        NOT NULL,

    topic_count         smallint     NOT NULL CHECK (topic_count >= 0),

    -- Convenience-decoded topic[0] Symbol/String; NULL when topic[0]
    -- is not a Symbol or String XDR value (e.g. an Address-topic
    -- variant). Downstream decoders should NOT trust this column for
    -- correctness — read topic_0_xdr — but the index on it is the
    -- backfill fast-path.
    topic_0_sym         text,

    -- Raw XDR bytes of the topic slots. topic_0_xdr is NOT NULL
    -- because every Soroban contract event has at least one topic;
    -- topics 1-3 are NULL when not present.
    topic_0_xdr         bytea        NOT NULL,
    topic_1_xdr         bytea,
    topic_2_xdr         bytea,
    topic_3_xdr         bytea,

    -- Raw XDR bytes of the event body (SCVal — could be Map, Vec,
    -- Bytes, scalar, or anything else the contract emits).
    body_xdr            bytea        NOT NULL,

    -- Concatenated raw XDR bytes of the originating InvokeContract
    -- op's arguments, when applicable. NULL when the event came from
    -- a non-InvokeContract op (system events, CAP-67 classic-op
    -- transfer events, etc.). Powers decoders like RedStone that
    -- need the op args (feed_ids) alongside the event body.
    op_args_xdr         bytea,

    PRIMARY KEY (ledger, tx_hash, op_index, event_index)
);

COMMENT ON TABLE soroban_events IS
    'Raw-event landing zone — every Soroban contract event the '
    'dispatcher routes is also captured here. Future per-source '
    'decoder backfills run as INSERT ... SELECT FROM soroban_events '
    'WHERE contract_id IN (...) AND topic_0_sym IN (...) rather '
    'than MinIO re-walks. See ADR-0029.';
COMMENT ON COLUMN soroban_events.topic_0_sym IS
    'Convenience-decoded topic[0] Symbol/String; NULL when topic[0] '
    'is not a Symbol or String XDR value. Decoders MUST read '
    'topic_0_xdr for correctness — this column is for index fast-paths.';
COMMENT ON COLUMN soroban_events.op_args_xdr IS
    'Concatenated raw XDR of the originating InvokeContract op''s '
    'arguments when applicable; NULL for non-InvokeContract events.';

SELECT create_hypertable(
    'soroban_events',
    'ledger_close_time',
    chunk_time_interval => INTERVAL '1 day',
    if_not_exists       => TRUE
);

-- (1) Per-contract walk; primary backfill query shape.
CREATE INDEX soroban_events_contract_ts_idx
    ON soroban_events (contract_id, ledger_close_time DESC);

-- (2) Cross-contract symbol scan.
CREATE INDEX soroban_events_topic_sym_ts_idx
    ON soroban_events (topic_0_sym, ledger_close_time DESC)
    WHERE topic_0_sym IS NOT NULL;

-- (3) Per-decoder fast filter ("this contract + this symbol").
CREATE INDEX soroban_events_contract_topic_idx
    ON soroban_events (contract_id, topic_0_sym)
    WHERE topic_0_sym IS NOT NULL;

-- Compression — segment-by contract_id since backfills group by it,
-- order-by ledger_close_time DESC so newest-first per-segment.
ALTER TABLE soroban_events SET (
    timescaledb.compress,
    timescaledb.compress_segmentby = 'contract_id',
    timescaledb.compress_orderby   = 'ledger_close_time DESC'
);

-- Compress chunks older than 7 days. Same horizon as the existing
-- hypertables (trades, oracle_updates) and a comfortable headroom
-- before TimescaleDB's compaction interferes with backfills that
-- might re-target a recent chunk.
SELECT add_compression_policy(
    'soroban_events',
    INTERVAL '7 days',
    if_not_exists => TRUE
);

COMMIT;
