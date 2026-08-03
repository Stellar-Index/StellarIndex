-- 0129 up — `aquarius_protocol_fee` hypertable.
--
-- Aquarius pools emit two treasury/governance events that were
-- classify()'d but Matches()-dropped (recognized-only) until
-- 2026-08-03 — the raw events sat in the ClickHouse lake unserved.
-- This table + the decoder arms close that every-event gap.
--
-- Two events, ONE table with a `kind` discriminator (the field sets
-- diverge but both are "protocol fee" treasury events; per-kind
-- nullable typed columns, NOT a jsonb blob):
--
--   set_protocol_fee   — governance sets the protocol fee bps. Body:
--                        Map{ fee_protocol0_new, fee_protocol0_old,
--                             fee_protocol1_new, fee_protocol1_old : u32 }
--                        (the per-token old→new fee transition). Rare
--                        (3 events lake-wide).
--   claim_protocol_fee — the accrued protocol fee is swept to the
--                        recipient. Body: Vec[ recipient: Address,
--                        amount: i128 ]. One event per token claimed
--                        (event_index disambiguates the per-tx pair);
--                        the token identity is positional/not in the
--                        body. 2,530 events / 99 pools.
--
-- Storage shape: per-protocol table, pool-gated (ADR-0035, same trust
-- root as trade/reserves). Aquarius has no published price; these rows
-- never reach VWAP. derive_generation is the INV-3 generation-guarded
-- corrective-upsert column (migration 0110).
--
-- Retention: NONE — granular-coverage mission keeps the history forever.
--
-- Historical fill: `stellarindex-ops projector-replay -source aquarius`
-- re-derives both from the lake (ADR-0034).

BEGIN;

CREATE TABLE aquarius_protocol_fee (
    -- Emitting pool contract C-strkey.
    contract_id        text         NOT NULL,

    -- Soroban event identity.
    ledger             integer      NOT NULL CHECK (ledger >= 0),
    ledger_close_time  timestamptz  NOT NULL,
    tx_hash            text         NOT NULL,
    op_index           integer      NOT NULL CHECK (op_index >= 0),
    event_index        integer      NOT NULL CHECK (event_index >= 0),

    kind               text         NOT NULL CHECK (kind IN (
        'set_protocol_fee', 'claim_protocol_fee')),

    -- set_protocol_fee columns (NULL on claim rows). The per-token
    -- old→new protocol-fee values (u32 on the wire). NUMERIC per
    -- ADR-0003 (monetary column) even though the fee bps fits an
    -- integer — the invariant is uniform and future-proofs a larger fee.
    fee_protocol0_new  numeric      CHECK (fee_protocol0_new IS NULL OR fee_protocol0_new >= 0),
    fee_protocol0_old  numeric      CHECK (fee_protocol0_old IS NULL OR fee_protocol0_old >= 0),
    fee_protocol1_new  numeric      CHECK (fee_protocol1_new IS NULL OR fee_protocol1_new >= 0),
    fee_protocol1_old  numeric      CHECK (fee_protocol1_old IS NULL OR fee_protocol1_old >= 0),

    -- claim_protocol_fee columns (NULL on set rows). recipient is the
    -- fee-sweep destination; amount the swept i128 (NUMERIC per
    -- ADR-0003, never truncates). One row per token claimed.
    recipient          text,
    amount             numeric      CHECK (amount IS NULL OR amount >= 0),

    derive_generation  integer      NOT NULL DEFAULT 0,
    ingested_at        timestamptz  NOT NULL DEFAULT now(),

    -- PK includes ledger_close_time (TimescaleDB TS103). event_index
    -- disambiguates the two per-token claim_protocol_fee events in one
    -- op, and a set + claim folded onto the same op.
    PRIMARY KEY (ledger_close_time, contract_id, ledger, tx_hash,
                 op_index, event_index)
);

COMMENT ON TABLE aquarius_protocol_fee IS
    'Aquarius protocol-fee treasury events (set_protocol_fee / '
    'claim_protocol_fee), discriminated by kind. No published price; '
    'never contributes to VWAP. Hypertable on ledger_close_time. See '
    'internal/sources/aquarius/README.md.';
COMMENT ON COLUMN aquarius_protocol_fee.recipient IS
    'claim_protocol_fee only — the fee-sweep destination. The claimed '
    'token is positional (one row per token); join a recent trade / '
    'deposit_liquidity for the pool to resolve it.';

SELECT create_hypertable(
    'aquarius_protocol_fee',
    'ledger_close_time',
    chunk_time_interval => INTERVAL '7 days',
    if_not_exists       => TRUE
);

-- Per-pool walk ("this pool's fee events, newest first").
CREATE INDEX aquarius_protocol_fee_contract_ts_idx
    ON aquarius_protocol_fee (contract_id, ledger_close_time DESC);

-- Per-kind cross-pool scan ("recent fee claims across Aquarius").
CREATE INDEX aquarius_protocol_fee_kind_ts_idx
    ON aquarius_protocol_fee (kind, ledger_close_time DESC);

ALTER TABLE aquarius_protocol_fee SET (
    timescaledb.compress,
    timescaledb.compress_segmentby = 'contract_id, kind',
    timescaledb.compress_orderby   = 'ledger_close_time DESC, ledger DESC'
);

SELECT add_compression_policy(
    'aquarius_protocol_fee',
    INTERVAL '7 days',
    if_not_exists => TRUE
);

COMMIT;
