-- 0146 up — `defindex_fees` hypertable (W5.2).
--
-- The DeFindex vault-layer `("DeFindexVault","dfees")` event was
-- classify()'d but recognised-and-dropped (no decoder, no table) since
-- Phase B — blocked on a real on-chain body sample per the do-not-invent
-- discipline. That sample is now captured and proven from the r1 lake
-- blobs (2026-08, decoded with internal/scval):
--
--   topics = (String "DeFindexVault", Symbol "dfees"), topic_count = 2
--   body   = Map{ distributed_fees: Vec[ (token Address<contract>, amount i128) ] }
--
-- PER-ASSET, not per-recipient: each Vec entry is a fee token contract
-- (e.g. USDC's SAC) + the i128 amount distributed in that token. The
-- Vec has 0..N entries — an EMPTY vec is a real observed shape (a
-- distribution ran with nothing to distribute) and produces zero rows.
--
-- Lake facts at capture time: 12,785 events on 27 vault contracts,
-- ledgers 60,903,337 → tip, still firing live. Every sample fires in
-- the SAME op as the vault deposit/withdraw flow (op_index 0,
-- event_index 5 in the captured set) — and one event fans out to one
-- row PER Vec entry (`fee_index` discriminator), a shape
-- defindex_flows' one-row-per-flow schema doesn't carry. Hence its own
-- table rather than a third defindex_flows layer.
--
-- One row per distributed_fees entry, so the ADR-0033 projection
-- reconcile counts 1:1 (the decoder emits one consumer event per
-- entry; empty vecs emit zero events and zero rows — count-consistent
-- by construction, no fan-out waiver needed).
--
-- derive_generation is the INV-3 generation-guarded corrective-upsert
-- column (migration 0110 convention; bigint per 0142 — it carries
-- time.Now().Unix(), int4 overflows 2038-01-19).
--
-- Retention: NONE — granular-coverage mission keeps the history forever.
--
-- Historical fill: `stellarindex-ops projector-replay -source defindex`
-- re-derives from the lake (ADR-0034).

BEGIN;

CREATE TABLE defindex_fees (
    -- Soroban event identity (0050/0055 defindex_flows conventions).
    ledger              integer      NOT NULL CHECK (ledger >= 0),
    ledger_close_time   timestamptz  NOT NULL,
    tx_hash             text         NOT NULL,
    op_index            smallint     NOT NULL CHECK (op_index >= 0),
    event_index         smallint     NOT NULL CHECK (event_index >= 0),

    -- Position of this entry in the event's distributed_fees Vec —
    -- the per-row discriminator for the one-event → N-entries fan-out.
    fee_index           smallint     NOT NULL CHECK (fee_index >= 0), -- lint-money:ok fee_index is the Vec-position PK discriminator, not a money value

    -- The emitting DeFindex vault-wrapper contract C-strkey.
    contract_id         text         NOT NULL,

    -- The fee token contract C-strkey (per-asset; sample tokens are
    -- SACs, e.g. USDC's CCW67TSZ…MI75).
    token               text         NOT NULL,

    -- Distributed fee amount in `token` (i128; NUMERIC per ADR-0003,
    -- never truncates).
    amount              numeric      NOT NULL CHECK (amount >= 0),

    derive_generation   bigint       NOT NULL DEFAULT 0,
    ingested_at         timestamptz  NOT NULL DEFAULT now(),

    -- PK includes ledger_close_time (TimescaleDB TS103, 0041's lesson).
    -- event_index + fee_index discriminate the same-op vault-flow
    -- sibling events and the per-entry fan-out respectively.
    PRIMARY KEY (ledger_close_time, contract_id, ledger, tx_hash,
                 op_index, event_index, fee_index)
);

COMMENT ON TABLE defindex_fees IS
    'DeFindex vault dfees distributions: one row per distributed_fees '
    'Vec entry (per fee TOKEN, not per recipient). Fires in the same op '
    'as the vault deposit/withdraw flow in defindex_flows (correlate by '
    'tx_hash + op_index). Hypertable on ledger_close_time. See '
    'internal/sources/defindex/README.md.';
COMMENT ON COLUMN defindex_fees.fee_index IS
    'Position of this entry in the event''s distributed_fees Vec — '
    'PK discriminator for the one-event → N-entries fan-out.';

SELECT create_hypertable(
    'defindex_fees',
    'ledger_close_time',
    chunk_time_interval => INTERVAL '30 days',
    if_not_exists       => TRUE
);

-- Same-tx correlation lookup (dfees ↔ the same-op vault flow).
CREATE INDEX defindex_fees_tx_hash_idx
    ON defindex_fees (tx_hash, ledger_close_time DESC);

-- Per-vault walk ("this vault's fee distributions, newest first").
CREATE INDEX defindex_fees_contract_ts_idx
    ON defindex_fees (contract_id, ledger_close_time DESC);

-- Per-token cross-vault scan ("recent USDC fee flow across DeFindex").
CREATE INDEX defindex_fees_token_ts_idx
    ON defindex_fees (token, ledger_close_time DESC);

ALTER TABLE defindex_fees SET (
    timescaledb.compress,
    timescaledb.compress_segmentby = 'contract_id',
    timescaledb.compress_orderby   = 'ledger_close_time DESC, ledger DESC'
);

SELECT add_compression_policy(
    'defindex_fees',
    INTERVAL '30 days',
    if_not_exists => TRUE
);

COMMIT;
