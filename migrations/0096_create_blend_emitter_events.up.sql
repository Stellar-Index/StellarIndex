-- 0095 up — `blend_emitter_events` hypertable.
--
-- One row per observed event from the Blend **Emitter** contract —
-- the protocol-emissions plumbing that mints/distributes BLND to the
-- backstop pools. Separate source from both `blend` (pool /
-- pool-factory) and `blend_backstop` (Backstop insurance module);
-- see internal/sources/blend_emitter/README.md.
--
-- Verified directly against the certified ClickHouse raw lake
-- (2026-07-09, ADR-0034): the Emitter's single canonical mainnet
-- contract (CCOQM6S7ICIUWA225O5PSJWUBEMXGFSSW2PQFO6FP4DQEKMS5DASRGRR)
-- has emitted exactly 469 events across its whole history, 4 distinct
-- topics, ALL single-topic:
--
--   distribute (465, 51,524,666→63,380,088) — one BLND emission to a
--               backstop. body = Vec[Address backstop_id, i128 amount].
--   drop       (2, ledgers 51,499,914 + 57,467,292) — a one-shot
--               airdrop to a VARIABLE-LENGTH recipient list (observed
--               arities: 13 and 3). body = Vec[Vec[Address, i128], ...].
--               ONE contract event fans out to N rows here —
--               recipient_index is the fan-out discriminator (same
--               "coarse PK collapses a multi-row emission" lesson
--               Phoenix/Aquarius already codify; see aquarius_reserves,
--               migration 0089, for the closest precedent).
--   q_swap     (1, ledger 56,992,670) — QUEUES a backstop-swap,
--               subject to a timelock. body = Map{new_backstop,
--               new_backstop_token, unlock_time}.
--   swap       (1, ledger 57,467,277) — EXECUTES a previously queued
--               backstop-swap. Same body shape as q_swap.
--
-- GATING (ADR-0035/0040): `distribute` COLLIDES with blend_backstop's
-- own `distribute` topic (different body shape — see
-- internal/sources/blend_emitter/events.go's package doc); the
-- decoder gates Matches() on contract identity (curated one-contract
-- set, comet.MainnetGatedSet() pattern — no factory namespace exists
-- for the Emitter to anchor a deploy-graph gate on).
--
-- Storage shape: per-protocol table, same decision taken for
-- cctp_events (0038) / comet_liquidity (0042) / aquarius_reserves
-- (0089). The Emitter has no published price; these rows never reach
-- the trades hypertable or VWAP.
--
-- Column shape mirrors comet_liquidity (0042): a handful of typed,
-- nullable, per-kind columns rather than a jsonb attributes blob —
-- the four kinds' fields are few enough (backstop_id/recipient/
-- amount/new_backstop/new_backstop_token/unlock_time) that typed
-- columns stay queryable without the jsonb indirection cctp_events
-- needed for its 26-kind vocabulary.
--
-- Identity: (contract_id, ledger, tx_hash, op_index, event_kind,
-- event_index, recipient_index). event_index distinguishes two
-- Emitter events landing on the same op (defensive — not observed,
-- but comet_liquidity/aquarius_reserves keep the same discriminator
-- for the same reason); recipient_index distinguishes the N rows a
-- single `drop` event fans out to (0 for the other three kinds, which
-- always produce exactly one row per event). ledger_close_time drags
-- into the PK because TimescaleDB requires the partition column there
-- (TS103).
--
-- Retention: NONE — granular-coverage mission keeps emissions history
-- forever.
--
-- Historical fill: live ingest writes here from deploy onward; the
-- back-window is re-derived from the raw lake with
--   stellarindex-ops projector-replay -source blend_emitter -from <genesis>
-- once BackfillSafe flips true (see README.md "Backfill safety" —
-- currently an open wasm-audit item across the Emitter's 3 observed
-- WASM uploads).

BEGIN;

CREATE TABLE blend_emitter_events (
    -- Emitting contract C-strkey. A single value today
    -- (blend_emitter.MainnetEmitter); the column exists so a future
    -- operator-admitted second instance (ADR-0040 curated-set seam)
    -- doesn't need a schema change.
    contract_id        text         NOT NULL,

    -- Soroban event identity.
    ledger             integer      NOT NULL CHECK (ledger >= 0),
    ledger_close_time  timestamptz  NOT NULL,
    tx_hash            char(64)     NOT NULL,
    op_index           integer      NOT NULL CHECK (op_index >= 0),
    -- Per-event discriminator within the op (an op can in principle
    -- emit more than one Emitter event). Same role as
    -- comet_liquidity.event_index / aquarius_reserves.event_index.
    event_index        integer      NOT NULL CHECK (event_index >= 0),

    -- Which of the four Emitter events this row represents. Pinned to
    -- the Event* constants in internal/sources/blend_emitter/events.go.
    event_kind         text         NOT NULL CHECK (event_kind IN (
        'distribute', 'drop', 'q_swap', 'swap')),

    -- Fan-out discriminator for `drop` (a single contract event's
    -- variable-length recipient Vec becomes one row per recipient,
    -- 0-indexed). Always 0 for the other three kinds, which each
    -- produce exactly one row per contract event.
    recipient_index    integer      NOT NULL DEFAULT 0 CHECK (recipient_index >= 0),

    -- distribute only: the backstop pool this emission went to.
    backstop_id        text,

    -- drop only: the recipient address at recipient_index.
    recipient          text,

    -- distribute + drop (per-row): the BLND amount moved. NUMERIC per
    -- ADR-0003 (i128 amounts never truncate to int64). NULL on
    -- q_swap/swap (those carry no amount).
    amount             numeric      CHECK (amount IS NULL OR amount > 0),

    -- q_swap + swap: the backstop (and its LP/BLND token) the Emitter
    -- is queuing/executing a change to point at.
    new_backstop        text,
    new_backstop_token  text,

    -- q_swap + swap: the timelock the queued swap is/was subject to.
    -- Stored as timestamptz (converted from the contract's u64 Unix-
    -- seconds field) — same convention as soroswap_router.deadline_ts
    -- (migration 0025), not a raw bigint, so operators can query it
    -- like every other timestamp column.
    unlock_time         timestamptz,

    ingested_at        timestamptz  NOT NULL DEFAULT now(),

    -- PK includes ledger_close_time (TimescaleDB partition-column
    -- requirement, TS103). event_kind + event_index + recipient_index
    -- together keep every fanned-out drop row distinct while staying
    -- a no-op (always 0) for the three single-row kinds.
    PRIMARY KEY (ledger_close_time, contract_id, ledger, tx_hash,
                 op_index, event_kind, event_index, recipient_index)
);

COMMENT ON TABLE blend_emitter_events IS
    'Per-event Blend Emitter protocol-emissions events (distribute / '
    'drop / q_swap / swap). No published price; never contributes to '
    'VWAP. Hypertable on ledger_close_time. See '
    'internal/sources/blend_emitter/README.md.';
COMMENT ON COLUMN blend_emitter_events.recipient_index IS
    'Fan-out discriminator for drop (one row per recipient in the '
    'event''s variable-length Vec). 0 for distribute/q_swap/swap, '
    'which always produce exactly one row per contract event.';
COMMENT ON COLUMN blend_emitter_events.unlock_time IS
    'q_swap/swap only: the backstop-swap timelock, converted from the '
    'contract''s u64 Unix-seconds field to timestamptz.';

SELECT create_hypertable(
    'blend_emitter_events',
    'ledger_close_time',
    chunk_time_interval => INTERVAL '7 days',
    if_not_exists       => TRUE
);

-- Per-contract walk ("every Emitter event, newest first").
CREATE INDEX blend_emitter_events_contract_time_idx
    ON blend_emitter_events (contract_id, ledger_close_time DESC);

-- Per-kind cross-contract scan ("recent distribute flow").
CREATE INDEX blend_emitter_events_kind_time_idx
    ON blend_emitter_events (event_kind, ledger_close_time DESC);

-- Per-backstop walk ("every emission this backstop received") —
-- partial index since backstop_id is distribute-only.
CREATE INDEX blend_emitter_events_backstop_time_idx
    ON blend_emitter_events (backstop_id, ledger_close_time DESC)
    WHERE backstop_id IS NOT NULL;

-- Compression after 7 days, segmented by contract_id (matches
-- comet_liquidity / soroban_events convention — TS103 sibling rule).
ALTER TABLE blend_emitter_events SET (
    timescaledb.compress,
    timescaledb.compress_segmentby = 'contract_id, event_kind',
    timescaledb.compress_orderby   = 'ledger_close_time DESC, ledger DESC'
);

SELECT add_compression_policy(
    'blend_emitter_events',
    INTERVAL '7 days',
    if_not_exists => TRUE
);

COMMIT;
