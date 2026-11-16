-- 0224 up — lossless BLNT V3 event catalogue.
--
-- One row per event accepted by the contract-identity-gated BLNT V3 decoder.
-- BLNT V3 is Blend-derived but has its own pool, tiered-backstop, emitter and
-- backfill ABIs; these rows intentionally do not enter the blend_* tables.
-- The topic-zero symbol is promoted to event_kind while every topic and the
-- body remain as exact base64 SCVal XDR. This keeps the initial projection
-- lossless and lets later typed lending-state projections be derived without
-- inventing V2-compatible semantics.
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
    event_kind text   NOT NULL CHECK (event_kind <> ''),
    topics_xdr text[] NOT NULL CHECK (cardinality(topics_xdr) >= 1),
    value_xdr  text   NOT NULL CHECK (value_xdr <> ''),

    derive_generation bigint      NOT NULL DEFAULT 0,
    ingested_at       timestamptz NOT NULL DEFAULT now(),

    PRIMARY KEY (ledger_close_time, contract_id, ledger, tx_hash,
                 op_index, event_index)
);

COMMENT ON TABLE blnt_events IS
    'Lossless, identity-gated BLNT V3 event catalogue. contract_role selects '
    'the immutable ABI; event_kind is topic[0]; topics_xdr/value_xdr preserve '
    'the complete base64 SCVal wire values. Not a Blend V1/V2 table and not a '
    'spot-price source.';

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
