-- 0203 up — `sushiswap_v3_position_events` hypertable.
--
-- One row per observed SushiSwap V3 pool `mint` | `burn` | `collect` event:
-- the concentrated-liquidity position lifecycle. Until now these were
-- recognised and gated but projected zero rows, so the raw events sat in
-- the ClickHouse lake unserved. A V3 position is (owner, tick_lower,
-- tick_upper); it does not fit the reserve-shaped soroswap_liquidity row,
-- so it gets its own table.
--
-- Wire shape (golden bodies from the certified lake, decoded BY FIELD
-- NAME; internal/sources/sushiswap_v3/decode.go decodePositionFields):
--
--   mint    { amount u128, amount0 u128, amount1 u128, owner Address,
--             sender Address, tick_lower i32, tick_upper i32 }
--   burn    { amount u128, amount0 u128, amount1 u128, owner Address,
--             tick_lower i32, tick_upper i32 }
--   collect { amount0 u128, amount1 u128, owner Address,
--             tick_lower i32, tick_upper i32 [, recipient Address] }
--
--   liquidity            — `amount` of a mint / burn: the liquidity delta
--                          added / removed. NULL for collect (a collect
--                          moves owed tokens, not liquidity).
--   amount_0 / amount_1  — token0 / token1 amounts the event moved.
--   owner                — the position owner.
--   sender               — mint only: the caller that funded it.
--   recipient            — collect only: who received the tokens.
--
-- token_0 / token_1 are NOT in the event body; they come from the pool
-- registry (the factory's `pool_created`) at decode time. A pool gated in
-- without a token mapping fails CLOSED into a counted decode error, so
-- both columns are NOT NULL — a row is never written with invented assets.
--
-- Concentrated liquidity: these rows carry no price and never reach
-- trades or VWAP; no reserve- or TVL-shaped value is derivable from them.
--
-- Identity: (pool, ledger, tx_hash, op_index, event_index, action).
-- event_index drags in because one operation can emit several position
-- events on a pool; action drags in so a mint + burn folded onto one
-- event slot cannot collide. ledger_close_time drags in because
-- TimescaleDB requires the partition column in every unique index on a
-- hypertable.
--
-- derive_generation: the generation-guarded corrective-upsert column
-- (migrations 0110 / 0142 — bigint, no 2038 cliff), so a corrected
-- projector re-derive lands in place and a live gen-0 replay cannot
-- revert it.
--
-- Retention: NONE — granular-coverage mission keeps position history.
--
-- Historical fill: `stellarindex-ops projector-replay -source
-- sushiswap_v3 -from <FactoryGenesisLedger>` re-derives the events from
-- the ClickHouse lake (ADR-0034).
--
-- REQUIRED-FOLLOWUP: decompress trades chunks, then projector-replay -source sushiswap_v3 -from 61487379

BEGIN;

CREATE TABLE IF NOT EXISTS sushiswap_v3_position_events (
    -- Emitting pool contract C-strkey.
    pool               text         NOT NULL,

    -- Soroban event identity.
    ledger             integer      NOT NULL CHECK (ledger >= 0),
    ledger_close_time  timestamptz  NOT NULL,
    tx_hash            bytea        NOT NULL, -- 32-byte raw hash
    op_index           smallint     NOT NULL CHECK (op_index >= 0),
    event_index        smallint     NOT NULL CHECK (event_index >= 0),

    action             text         NOT NULL CHECK (action IN ('mint', 'burn', 'collect')),

    owner              text         NOT NULL,
    sender             text,
    recipient          text,

    -- Pool token identities (canonical asset ids) from the pool registry.
    token_0            text         NOT NULL,
    token_1            text         NOT NULL,

    -- Position tick range. i32 on the wire; not ordered-checked here so an
    -- unexpected on-chain value is stored, not refused.
    tick_lower         integer      NOT NULL,
    tick_upper         integer      NOT NULL,

    -- NUMERIC per ADR-0003 (u128 never truncates to int64).
    liquidity          numeric      CHECK (liquidity >= 0),
    amount_0           numeric      NOT NULL CHECK (amount_0 >= 0),
    amount_1           numeric      NOT NULL CHECK (amount_1 >= 0),

    derive_generation  bigint       NOT NULL DEFAULT 0,
    ingested_at        timestamptz  NOT NULL DEFAULT now(),

    CHECK (action = 'collect' OR liquidity IS NOT NULL),

    PRIMARY KEY (ledger_close_time, pool, ledger, tx_hash, op_index,
                 event_index, action)
);

COMMENT ON TABLE sushiswap_v3_position_events IS
    'Per-event SushiSwap V3 pool mint / burn / collect (concentrated-'
    'liquidity position lifecycle) rows. Carry no price; never '
    'contribute to VWAP. Hypertable on ledger_close_time. See '
    'internal/sources/sushiswap_v3/decode.go decodePositionFields.';
COMMENT ON COLUMN sushiswap_v3_position_events.liquidity IS
    'Liquidity delta (the event body''s `amount`) for mint / burn; NULL '
    'for collect.';

SELECT create_hypertable(
    'sushiswap_v3_position_events',
    'ledger_close_time',
    chunk_time_interval => INTERVAL '7 days',
    if_not_exists       => TRUE
);

-- Per-pool walk ("every position event on this pool, newest first").
CREATE INDEX IF NOT EXISTS sushiswap_v3_position_events_pool_time_idx
    ON sushiswap_v3_position_events (pool, ledger_close_time DESC);

-- Per-owner walk ("an LP's positions across pools").
CREATE INDEX IF NOT EXISTS sushiswap_v3_position_events_owner_time_idx
    ON sushiswap_v3_position_events (owner, ledger_close_time DESC);

-- Cross-pool per-action scan.
CREATE INDEX IF NOT EXISTS sushiswap_v3_position_events_action_time_idx
    ON sushiswap_v3_position_events (action, ledger_close_time DESC);

ALTER TABLE sushiswap_v3_position_events SET (
    timescaledb.compress,
    timescaledb.compress_segmentby = 'pool, action',
    timescaledb.compress_orderby   = 'ledger_close_time DESC, ledger DESC'
);

SELECT add_compression_policy(
    'sushiswap_v3_position_events',
    INTERVAL '7 days',
    if_not_exists => TRUE
);

COMMIT;
