-- 0049 up — soroswap_router_swaps hypertable.
--
-- Captures one row per Soroswap router invocation (= one call to
-- `swap_exact_tokens_for_tokens` / `swap_tokens_for_exact_tokens`).
-- The router emits no events of its own; this table is populated by
-- the `dispatcher.ContractCallDecoder` path that reads
-- InvokeContract op args directly. See internal/sources/soroswap_router/.
--
-- Sister table to `trades` (per-pair leg-level swaps from the
-- soroswap pair contracts). A single router invocation appears as
-- one row here AND N per-pair `trades` rows with the same
-- (ledger, tx_hash, op_index). The relationship is many-trades-per-
-- router-call when path length > 2 (multi-hop swaps).
--
-- Until this migration the consumer was a stub (the events decoded
-- and logged but never persisted). Without persistence there is no
-- gap-detector signal for the source — the status page showed 0%.
-- This migration is the storage half of getting soroswap-router to
-- honest 100% coverage; the consumer wire-up + per-source gap-
-- detector target ship in the same PR.
--
-- Identity per dispatcher invariants:
--   (ledger, tx_hash, op_index) uniquely identifies one InvokeContract
--   operation on Stellar. ledger_close_time drags into the PK
--   because TimescaleDB requires the partition column in every
--   unique index on a hypertable (TS103, see 0041's lesson).
--
-- Retention: NONE. Granular-coverage mission keeps router-level
-- attribution (the `routed_via` follow-up against `trades.routed_via`
-- joins through this table).

BEGIN;

CREATE TABLE soroswap_router_swaps (
    ledger            integer      NOT NULL CHECK (ledger >= 0),
    ledger_close_time timestamptz  NOT NULL,
    tx_hash           text         NOT NULL,
    op_index          smallint     NOT NULL CHECK (op_index >= 0),

    contract_id       text         NOT NULL,
    function_name     text         NOT NULL CHECK (function_name IN (
        'swap_exact_tokens_for_tokens',
        'swap_tokens_for_exact_tokens')),

    -- Originator (op source) + tx source. Both G-strkey or muxed.
    op_source         text,
    tx_source         text,

    recipient         text         NOT NULL,
    -- The hop sequence the router walked. Array of token contract
    -- C-strkeys (canonical addresses). Length >= 2 by router
    -- precondition.
    path              text[]       NOT NULL CHECK (cardinality(path) >= 2),

    -- exact-input or upper-bound (depending on function_name).
    -- NUMERIC per ADR-0003 — i128 never truncates.
    amount_in         numeric      NOT NULL CHECK (amount_in >= 0),
    -- exact-output or lower-bound (depending on function_name).
    amount_out        numeric      NOT NULL CHECK (amount_out >= 0),

    deadline_ts       timestamptz,

    ingested_at       timestamptz  NOT NULL DEFAULT now(),

    PRIMARY KEY (ledger_close_time, ledger, tx_hash, op_index)
);

-- Hypertable with 30-day chunks (matches the post-merge `trades`
-- interval — see [[chunk-consolidation-2026-06-01]] memory).
SELECT create_hypertable(
    'soroswap_router_swaps',
    'ledger_close_time',
    chunk_time_interval => INTERVAL '30 days'
);

-- Lookup-by-tx (router → underlying pair swaps in `trades`).
CREATE INDEX soroswap_router_swaps_tx_hash_idx
    ON soroswap_router_swaps (tx_hash, ledger_close_time DESC);

-- Lookup-by-recipient (per-user attribution).
CREATE INDEX soroswap_router_swaps_recipient_ts_idx
    ON soroswap_router_swaps (recipient, ledger_close_time DESC);

-- Compression eligibility (segment by recipient to keep per-user
-- range queries efficient).
ALTER TABLE soroswap_router_swaps SET (
    timescaledb.compress,
    timescaledb.compress_segmentby = 'recipient',
    timescaledb.compress_orderby = 'ledger_close_time DESC, ledger DESC'
);

COMMIT;
