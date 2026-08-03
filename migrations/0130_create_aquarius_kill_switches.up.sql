-- 0130 up — `aquarius_kill_switches` hypertable.
--
-- Aquarius pools emit circuit-breaker toggle events that were
-- classify()'d but Matches()-dropped (recognized-only) until
-- 2026-08-03 — the raw events sat in the ClickHouse lake unserved.
-- This table + the decoder arm close that every-event gap.
--
-- Eight kinds — kill_/unkill_ pairs for deposit, swap, claim, and
-- gauges_claim — an admin pausing / unpausing a pool operation. Six of
-- the eight are observed on mainnet (32 events lake-wide; the two
-- gauges_claim variants have 0 to date, admitted defensively so they
-- land if they ever fire).
--
-- Wire shape (lake-verified): topic[0] = Symbol(action) is the ONLY
-- topic; body = SCV_VOID (empty). These are pure toggle signals — the
-- event carries no amount, address, or field data, so the row is the
-- identity + the action. There is nothing to decode from the body.
--
-- Storage shape: per-protocol table, pool-gated (ADR-0035). No amounts,
-- no VWAP contribution. derive_generation is the INV-3 generation-
-- guarded corrective-upsert column (migration 0110).
--
-- Retention: NONE — granular-coverage mission keeps the history forever.
--
-- Historical fill: `stellarindex-ops projector-replay -source aquarius`.

BEGIN;

CREATE TABLE aquarius_kill_switches (
    -- Emitting pool contract C-strkey.
    contract_id        text         NOT NULL,

    -- Soroban event identity.
    ledger             integer      NOT NULL CHECK (ledger >= 0),
    ledger_close_time  timestamptz  NOT NULL,
    tx_hash            text         NOT NULL,
    op_index           integer      NOT NULL CHECK (op_index >= 0),
    event_index        integer      NOT NULL CHECK (event_index >= 0),

    -- Which circuit-breaker toggle fired.
    action             text         NOT NULL CHECK (action IN (
        'kill_deposit', 'unkill_deposit',
        'kill_swap', 'unkill_swap',
        'kill_claim', 'unkill_claim',
        'kill_gauges_claim', 'unkill_gauges_claim')),

    derive_generation  integer      NOT NULL DEFAULT 0,
    ingested_at        timestamptz  NOT NULL DEFAULT now(),

    -- PK includes ledger_close_time (TimescaleDB TS103). event_index so
    -- two toggles folded onto one op don't collide.
    PRIMARY KEY (ledger_close_time, contract_id, ledger, tx_hash,
                 op_index, event_index)
);

COMMENT ON TABLE aquarius_kill_switches IS
    'Aquarius pool circuit-breaker toggles (kill_/unkill_ for deposit / '
    'swap / claim / gauges_claim). Pure toggle signals — no body data. '
    'No published price; never contributes to VWAP. Hypertable on '
    'ledger_close_time. See internal/sources/aquarius/README.md.';

SELECT create_hypertable(
    'aquarius_kill_switches',
    'ledger_close_time',
    chunk_time_interval => INTERVAL '7 days',
    if_not_exists       => TRUE
);

-- Per-pool walk ("this pool's pause/unpause history").
CREATE INDEX aquarius_kill_switches_contract_ts_idx
    ON aquarius_kill_switches (contract_id, ledger_close_time DESC);

-- Cross-pool per-action scan ("recent deposit pauses across Aquarius").
CREATE INDEX aquarius_kill_switches_action_ts_idx
    ON aquarius_kill_switches (action, ledger_close_time DESC);

ALTER TABLE aquarius_kill_switches SET (
    timescaledb.compress,
    timescaledb.compress_segmentby = 'contract_id, action',
    timescaledb.compress_orderby   = 'ledger_close_time DESC, ledger DESC'
);

SELECT add_compression_policy(
    'aquarius_kill_switches',
    INTERVAL '7 days',
    if_not_exists => TRUE
);

COMMIT;
