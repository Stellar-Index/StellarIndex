-- 0224 up — decoded BLNT V3 event history.
--
-- One row per event accepted by the contract-identity-gated BLNT V3 decoder.
-- BLNT V3 is Blend-derived but has its own pool, tiered-backstop, emitter and
-- backfill ABIs; these rows intentionally do not enter the blend_* tables.
-- Every event is decoded against its ABI at ingest: the fields readers filter
-- or sum on are columns, the rest of the payload is `attributes` under the
-- contract's own field names. The raw XDR is not copied here; it stays in the
-- ClickHouse lake, the replay source (ADR-0034).
--
-- Amounts are i128 on chain: NUMERIC columns, decimal strings inside
-- `attributes` (ADR-0003). Signed, so no non-negative CHECK.
--
-- No retention policy: granular protocol history is kept indefinitely.
-- Historical replay is fail-closed through the per-WASM audit documented in
-- internal/sources/blnt/README.md.

BEGIN;

CREATE TABLE blnt_events (
    contract_id       text        NOT NULL,
    ledger            integer     NOT NULL CHECK (ledger >= 0),
    ledger_close_time timestamptz NOT NULL,
    tx_hash           char(64)    NOT NULL,
    op_index          integer     NOT NULL CHECK (op_index >= 0),
    event_index       integer     NOT NULL CHECK (event_index >= 0),

    contract_role text NOT NULL CHECK (contract_role IN (
        'pool', 'factory', 'backstop', 'emitter', 'backfill', 'lockup_backfill'
    )),
    event_kind   text NOT NULL CHECK (event_kind <> ''),
    pool         text,
    user_address text,
    asset        text,
    tier         text CHECK (tier IN ('FirstLoss', 'SecondLoss', 'ThirdLoss')),
    amount       numeric,
    amount2      numeric,
    attributes   jsonb NOT NULL DEFAULT '{}'::jsonb,

    derive_generation bigint      NOT NULL DEFAULT 0,
    ingested_at       timestamptz NOT NULL DEFAULT now(),

    PRIMARY KEY (ledger_close_time, contract_id, ledger, tx_hash,
                 op_index, event_index)
);

COMMENT ON TABLE blnt_events IS
    'Decoded, identity-gated BLNT V3 event history. contract_role selects '
    'the ABI; event_kind is topic[0]. pool is the pool the event concerns '
    '(the emitting contract for role pool). amount/amount2 are the event''s '
    'primary and secondary i128 (internal/sources/blnt/abi.go names them per '
    'kind); attributes holds every other decoded field. Not a Blend V1/V2 '
    'table and not a spot-price source.';

SELECT create_hypertable(
    'blnt_events',
    'ledger_close_time',
    chunk_time_interval => INTERVAL '30 days',
    if_not_exists       => TRUE
);

CREATE INDEX blnt_events_contract_ts_idx
    ON blnt_events (contract_id, ledger_close_time DESC);

CREATE INDEX blnt_events_role_kind_ts_idx
    ON blnt_events (contract_role, event_kind, ledger_close_time DESC);

CREATE INDEX blnt_events_tx_hash_idx
    ON blnt_events (tx_hash, ledger_close_time DESC);

COMMIT;
