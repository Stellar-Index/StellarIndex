-- 0131 up — `phoenix_initialize` hypertable.
--
-- Phoenix pool contracts emit two `initialize` events at deploy — one
-- per pool token — that were classify()'d (actionInitialize) but
-- dropped at dispatcher_adapter.go (case actionInitialize → nil) until
-- 2026-08-03. The raw events sat in the ClickHouse lake unserved. This
-- table + the decoder arm close that every-event gap.
--
-- Wire shape (lake-verified, 40 events / 19 pools):
--   topic[0] = String("initialize")
--   topic[1] = String("XYK LP token_a" | "XYK LP token_b")  → token_slot
--   body     = Address(token contract)                      → token
--
-- So each pool deploy emits token_a + token_b as two rows sharing the
-- (pool, ledger, tx_hash, op_index) triple, distinguished by
-- event_index / token_slot.
--
-- Storage shape: per-protocol table, pool-gated (ADR-0035 — the same
-- curated phoenix set as trades). No amounts, no VWAP. derive_generation
-- is the INV-3 generation-guarded corrective-upsert column (0110).
--
-- Retention: NONE — granular-coverage mission keeps the history forever.
--
-- Historical fill: `stellarindex-ops projector-replay -source phoenix`.

BEGIN;

CREATE TABLE phoenix_initialize (
    -- Emitting pool contract C-strkey.
    pool               text         NOT NULL,

    -- Soroban event identity.
    ledger             integer      NOT NULL CHECK (ledger >= 0),
    ledger_close_time  timestamptz  NOT NULL,
    tx_hash            text         NOT NULL,
    op_index           smallint     NOT NULL CHECK (op_index >= 0),
    event_index        smallint     NOT NULL CHECK (event_index >= 0),

    -- Which pool token this row announces ('a' or 'b'), from topic[1].
    token_slot         char(1)      NOT NULL CHECK (token_slot IN ('a', 'b')),

    -- The announced token contract address (event body).
    token              text         NOT NULL,

    derive_generation  integer      NOT NULL DEFAULT 0,
    ingested_at        timestamptz  NOT NULL DEFAULT now(),

    -- PK includes ledger_close_time (TimescaleDB TS103). event_index +
    -- token_slot keep the two per-init rows distinct.
    PRIMARY KEY (ledger_close_time, pool, ledger, tx_hash,
                 op_index, event_index)
);

COMMENT ON TABLE phoenix_initialize IS
    'Phoenix pool-deploy initialize events — the pool''s two token '
    'announcements (token_a / token_b). No published price; never '
    'contributes to VWAP. Hypertable on ledger_close_time. See '
    'internal/sources/phoenix/decode.go.';

SELECT create_hypertable(
    'phoenix_initialize',
    'ledger_close_time',
    chunk_time_interval => INTERVAL '7 days',
    if_not_exists       => TRUE
);

-- Per-pool walk ("this pool's token set at deploy").
CREATE INDEX phoenix_initialize_pool_ts_idx
    ON phoenix_initialize (pool, ledger_close_time DESC);

-- Per-token walk ("which pools were deployed against this token").
CREATE INDEX phoenix_initialize_token_ts_idx
    ON phoenix_initialize (token, ledger_close_time DESC);

ALTER TABLE phoenix_initialize SET (
    timescaledb.compress,
    timescaledb.compress_segmentby = 'pool',
    timescaledb.compress_orderby   = 'ledger_close_time DESC, ledger DESC'
);

SELECT add_compression_policy(
    'phoenix_initialize',
    INTERVAL '7 days',
    if_not_exists => TRUE
);

COMMIT;
