-- 0128 up — `aquarius_reserves_sync` hypertable.
--
-- Aquarius pools emit a `reserves_sync` event IN ADDITION to
-- `update_reserves`. Until 2026-08-03 it was classify()'d but
-- Matches() returned false, so it projected no row (recognized-only) —
-- the raw events sat in the ClickHouse lake unserved. This table + the
-- decoder arm close that every-event gap.
--
-- reserves_sync is NOT redundant with update_reserves: verified on the
-- same pool + tx (CCRULRY3…, ledger 61,572,402) the two carry DIFFERENT
-- values — update_reserves[0] = 3,684,217 (post-state reserve) vs
-- reserves_sync = [11,126,447,651, 11,177,552,170] (decoder-verified). The
-- exact semantics of reserves_sync are per the Aquarius contract (a
-- distinct reserve-sync signal, likely the D-invariant / virtual
-- reserves used by the StableSwap math); we capture the vector
-- faithfully per token position without over-interpreting it.
--
-- Wire shape (lake-verified): body = `Vec<i128>` of per-position
-- values — the SAME shape as update_reserves, so the decoder reuses
-- decodeReserves() and fans one row per token position, exactly like
-- aquarius_reserves (0089).
--
-- Storage shape mirrors aquarius_reserves (per-protocol, fanned per
-- token position). Aquarius has no published price; these rows never
-- reach VWAP. derive_generation is the INV-3 generation-guarded
-- corrective-upsert column (migration 0110), same as aquarius_reserves.
--
-- Retention: NONE — granular-coverage mission keeps the history forever.
--
-- Historical fill: `stellarindex-ops projector-replay -source aquarius`
-- re-derives reserves_sync straight from the lake (ADR-0034).

BEGIN;

CREATE TABLE aquarius_reserves_sync (
    -- Emitting pool contract C-strkey.
    contract_id        text         NOT NULL,

    -- Soroban event identity.
    ledger             integer      NOT NULL CHECK (ledger >= 0),
    ledger_close_time  timestamptz  NOT NULL,
    tx_hash            text         NOT NULL,
    op_index           integer      NOT NULL CHECK (op_index >= 0),
    -- Per-event discriminator within the op (an op can emit several
    -- reserves_sync events — one per pool touched).
    event_index        integer      NOT NULL CHECK (event_index >= 0),

    -- Position of this value in the pool's canonical token order
    -- (0-based). token_index < pool token count.
    token_index        smallint     NOT NULL CHECK (token_index >= 0),

    -- The synced reserve value at token_index. NUMERIC per ADR-0003
    -- (i128 never truncates). The decoder rejects negatives; a value
    -- can transiently be 0, so the check is >= 0.
    reserve_synced     numeric      NOT NULL CHECK (reserve_synced >= 0),

    derive_generation  integer      NOT NULL DEFAULT 0,
    ingested_at        timestamptz  NOT NULL DEFAULT now(),

    -- PK includes ledger_close_time (TimescaleDB TS103). token_index
    -- drags in so the N fanned rows of one event stay distinct;
    -- event_index so two reserves_sync events in one op don't collide.
    PRIMARY KEY (ledger_close_time, contract_id, ledger, tx_hash,
                 op_index, event_index, token_index)
);

COMMENT ON TABLE aquarius_reserves_sync IS
    'Per-token Aquarius pool reserves_sync values (fanned one row per '
    'token position). A distinct reserve-sync signal, separate from '
    'update_reserves post-state (aquarius_reserves). No published price; '
    'never contributes to VWAP. Hypertable on ledger_close_time. See '
    'internal/sources/aquarius/README.md.';
COMMENT ON COLUMN aquarius_reserves_sync.reserve_synced IS
    'The reserves_sync value at this token position — semantics per the '
    'Aquarius contract (distinct from the update_reserves post-state); '
    'captured faithfully. reserves_sync carries no token address — join '
    'a recent deposit_liquidity / trade for the same pool to resolve it.';

SELECT create_hypertable(
    'aquarius_reserves_sync',
    'ledger_close_time',
    chunk_time_interval => INTERVAL '7 days',
    if_not_exists       => TRUE
);

-- Per-pool walk ("this pool's reserves_sync history, newest first").
CREATE INDEX aquarius_reserves_sync_contract_ts_idx
    ON aquarius_reserves_sync (contract_id, ledger_close_time DESC);

ALTER TABLE aquarius_reserves_sync SET (
    timescaledb.compress,
    timescaledb.compress_segmentby = 'contract_id',
    timescaledb.compress_orderby   = 'ledger_close_time DESC, ledger DESC'
);

SELECT add_compression_policy(
    'aquarius_reserves_sync',
    INTERVAL '7 days',
    if_not_exists => TRUE
);

COMMIT;
