-- 0050 up — defindex_flows hypertable.
--
-- One row per decoded DeFindex protocol flow event. Captures both
-- layers of the protocol:
--
-- - STRATEGY layer (`BlendStrategy` topic — fires from a strategy
--   contract on the underlying capital movement). `actor` here is
--   the vault contract C-strkey moving capital into / out of Blend.
--   The strategy layer does NOT carry end-user identity.
--
-- - VAULT layer (`DeFindexVault` topic — fires from a vault-wrapper
--   contract user-facing). `actor` here is the end-user G-strkey
--   (occasionally a C-strkey if routed via aggregator). Vault-layer
--   carries multi-asset `amounts` (the protocol supports
--   single-asset and multi-asset vaults) plus a share-token delta
--   (`df_tokens_minted` on deposit, `df_tokens_burned` on withdraw).
--
-- Unified single table with a `layer` discriminator + nullable
-- columns for the vault-only fields. This keeps the gap-detector
-- target shape consistent with every other Soroban source (one
-- table per source, with WhereFilter slicing layers/types if the
-- API ever needs to) and avoids a JOIN between two near-identical
-- tables when correlating strategy ↔ vault events in the same tx.
--
-- Pre-this-migration the dispatcher decoded these events but only
-- emitted INFO log lines — no persistence, no gap-detector signal,
-- the source was stuck at 0% on the status page. This migration is
-- the storage half of the EVERY-event policy
-- (project_every_event_principle) commitment.
--
-- Identity per Soroban event uniqueness: (contract_id, ledger,
-- tx_hash, op_index, event_index, layer). ledger_close_time drags
-- into the PK because TimescaleDB requires the partition column in
-- every unique index on a hypertable (TS103, see 0041's lesson).
--
-- Retention: NONE.

BEGIN;

CREATE TABLE defindex_flows (
    ledger              integer      NOT NULL CHECK (ledger >= 0),
    ledger_close_time   timestamptz  NOT NULL,
    tx_hash             text         NOT NULL,
    op_index            smallint     NOT NULL CHECK (op_index >= 0),

    contract_id         text         NOT NULL,

    -- Layer discriminator: 'strategy' or 'vault'.
    layer               text         NOT NULL CHECK (layer IN ('strategy', 'vault')),

    -- Direction: 'deposit' or 'withdraw'. Both layers use the same
    -- two-direction shape; the existing decoder normalises both.
    direction           text         NOT NULL CHECK (direction IN ('deposit', 'withdraw')),

    -- The address moving capital. Strategy layer: the vault
    -- contract C-strkey. Vault layer: the end-user G-strkey (or
    -- routing C-strkey).
    actor               text         NOT NULL,

    -- Strategy layer: single-asset amount (NUMERIC per ADR-0003).
    -- For vault layer this stays NULL — vault events carry a Vec
    -- in `amounts_vec` instead.
    amount              numeric      CHECK (amount IS NULL OR amount >= 0),

    -- Vault layer: per-asset deltas as a numeric array (one entry
    -- per asset in the vault's basket). NULL on strategy rows.
    amounts_vec         numeric[],

    -- Vault layer: share-token delta (df_tokens). NULL on strategy
    -- rows. NUMERIC per ADR-0003.
    df_tokens           numeric      CHECK (df_tokens IS NULL OR df_tokens >= 0),

    ingested_at         timestamptz  NOT NULL DEFAULT now(),

    PRIMARY KEY (ledger_close_time, contract_id, ledger, tx_hash, op_index, layer)
);

SELECT create_hypertable(
    'defindex_flows',
    'ledger_close_time',
    chunk_time_interval => INTERVAL '30 days'
);

-- Same-tx correlation lookup (strategy ↔ vault for one user op).
CREATE INDEX defindex_flows_tx_hash_idx
    ON defindex_flows (tx_hash, ledger_close_time DESC);

-- Per-actor history (end-user attribution at the vault layer).
CREATE INDEX defindex_flows_actor_ts_idx
    ON defindex_flows (actor, ledger_close_time DESC);

ALTER TABLE defindex_flows SET (
    timescaledb.compress,
    timescaledb.compress_segmentby = 'contract_id, layer',
    timescaledb.compress_orderby = 'ledger_close_time DESC, ledger DESC'
);

COMMIT;
