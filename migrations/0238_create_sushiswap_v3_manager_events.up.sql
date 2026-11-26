-- 0238 up — `sushiswap_v3_manager_events` hypertable.
--
-- One row per SushiSwap V3 position-manager event. The manager
-- (CARTUL5A…) opens every position it mints on a pool in its own name, so
-- the pool-side rows in sushiswap_v3_position_events carry the manager as
-- `owner`; this table is where a position (token_id) meets its end-user
-- owner and its liquidity, amount and fee movements.
--
-- Wire shape (post-upgrade bodies from the certified lake, decoded BY FIELD
-- NAME against an exact field set; internal/sources/sushiswap_v3/manager.go
-- managerShapes). Any other shape is a counted decode error, never a row:
--
--   mint               topics (mint, owner Address)  { token_id u32 }
--   burn               topics (burn, owner Address)  { token_id u32 }
--   increase_liquidity topics (increase_liquidity)
--                      { amount0 u128, amount1 u128, liquidity u128, token_id u32 }
--   decrease_liquidity same shape as increase_liquidity
--   collect            topics (collect)
--                      { amount0 u128, amount1 u128, recipient Address, token_id u32 }
--
-- No body names the pool. A manager event shares (ledger, tx_hash) with the
-- pool-side mint / burn / collect it drove, whose owner is the manager.
--
-- token_id is a u32 on the wire, so bigint (integer is signed 32-bit) with
-- the u32 range checked. Amounts and liquidity are u128: NUMERIC per
-- ADR-0003. A column the event kind does not carry is NULL, never zero; the
-- shape CHECKs pin which.
--
-- Identity: (manager, ledger, tx_hash, op_index, event_index, kind), with
-- ledger_close_time because TimescaleDB requires the partition column in
-- every unique index on a hypertable (as 0203).
--
-- derive_generation: the generation-guarded corrective-upsert column
-- (migrations 0110 / 0142), so a corrected re-derive lands in place and a
-- live gen-0 replay cannot revert it.
--
-- Retention: NONE — granular-coverage mission keeps position history.
--
-- New empty table: old binaries never touch it. History is filled by a
-- projected-rebuild of sushiswap_v3 once the manager is live in the gate
-- and its WASM hashes are audited (the replay gate refuses until then).

BEGIN;

CREATE TABLE IF NOT EXISTS sushiswap_v3_manager_events (
    -- Emitting position-manager contract C-strkey.
    manager            text         NOT NULL,

    -- Soroban event identity.
    ledger             integer      NOT NULL CHECK (ledger >= 0),
    ledger_close_time  timestamptz  NOT NULL,
    tx_hash            bytea        NOT NULL, -- 32-byte raw hash
    op_index           smallint     NOT NULL CHECK (op_index >= 0),
    event_index        smallint     NOT NULL CHECK (event_index >= 0),

    kind               text         NOT NULL CHECK (kind IN (
                           'mint', 'burn', 'increase_liquidity',
                           'decrease_liquidity', 'collect')),

    token_id           bigint       NOT NULL CHECK (token_id BETWEEN 0 AND 4294967295),

    -- mint / burn: topic[1], the position NFT's holder.
    owner              text,
    -- collect: who received the tokens.
    recipient          text,

    -- NUMERIC per ADR-0003 (u128 never truncates to int64).
    liquidity          numeric      CHECK (liquidity >= 0),
    amount_0           numeric      CHECK (amount_0 >= 0),
    amount_1           numeric      CHECK (amount_1 >= 0),

    derive_generation  bigint       NOT NULL DEFAULT 0,
    ingested_at        timestamptz  NOT NULL DEFAULT now(),

    CHECK ((owner IS NOT NULL) = (kind IN ('mint', 'burn'))),
    CHECK ((recipient IS NOT NULL) = (kind = 'collect')),
    CHECK ((liquidity IS NOT NULL) = (kind IN ('increase_liquidity', 'decrease_liquidity'))),
    CHECK ((amount_0 IS NOT NULL) = (kind NOT IN ('mint', 'burn'))),
    CHECK ((amount_1 IS NOT NULL) = (kind NOT IN ('mint', 'burn'))),

    PRIMARY KEY (ledger_close_time, manager, ledger, tx_hash, op_index,
                 event_index, kind)
);

COMMENT ON TABLE sushiswap_v3_manager_events IS
    'Per-event SushiSwap V3 position-manager mint / burn / increase_liquidity '
    '/ decrease_liquidity / collect rows: links a position token_id to its '
    'owner and its liquidity and amount movements. Carry no price; never '
    'contribute to VWAP. Hypertable on ledger_close_time. See '
    'internal/sources/sushiswap_v3/manager.go.';
COMMENT ON COLUMN sushiswap_v3_manager_events.token_id IS
    'Position NFT id (u32 on the wire).';
COMMENT ON COLUMN sushiswap_v3_manager_events.owner IS
    'mint / burn only: topic[1], the position holder at that event.';

SELECT create_hypertable(
    'sushiswap_v3_manager_events',
    'ledger_close_time',
    chunk_time_interval => INTERVAL '7 days',
    if_not_exists       => TRUE
);

-- Per-position walk ("everything that happened to token N").
CREATE INDEX IF NOT EXISTS sushiswap_v3_manager_events_token_time_idx
    ON sushiswap_v3_manager_events (manager, token_id, ledger_close_time DESC);

-- Per-owner walk ("an LP's positions").
CREATE INDEX IF NOT EXISTS sushiswap_v3_manager_events_owner_time_idx
    ON sushiswap_v3_manager_events (owner, ledger_close_time DESC)
    WHERE owner IS NOT NULL;

ALTER TABLE sushiswap_v3_manager_events SET (
    timescaledb.compress,
    timescaledb.compress_segmentby = 'manager, kind',
    timescaledb.compress_orderby   = 'ledger_close_time DESC, ledger DESC'
);

SELECT add_compression_policy(
    'sushiswap_v3_manager_events',
    INTERVAL '7 days',
    if_not_exists => TRUE
);

COMMIT;
