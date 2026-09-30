-- 0192 up — `defindex_admin_events` hypertable.
--
-- The DeFindex vault admin topics were classify()'d but dropped. Bodies
-- (decoded by field name from lake samples, see
-- test/fixtures/defindex/vault-admin-2026-09-30/):
--
--   rescue     {amount_withdrawn i128, caller, strategy_address}
--   paused     {caller, strategy_address}
--   unpaused   {caller, strategy_address}
--   nreceiver  {caller, new_fee_receiver}
--   nmanager   {new_manager}
--   nemanager  {new_emergency_manager}
--   rbmanager  {new_rebalance_manager}
--
-- One row per event, so the ADR-0033 reconcile counts 1:1. The shape
-- (nullable actor, per-kind columns) fits neither defindex_flows nor
-- defindex_fees, hence its own table in the phoenix_admin_events style.
--
-- derive_generation is the INV-3 generation-guarded upsert column,
-- bigint per 0142. Retention: NONE.
--
-- Historical fill: `stellarindex-ops projector-replay -source defindex`.

BEGIN;

CREATE TABLE defindex_admin_events (
    ledger              integer      NOT NULL CHECK (ledger >= 0),
    ledger_close_time   timestamptz  NOT NULL,
    tx_hash             text         NOT NULL,
    op_index            smallint     NOT NULL CHECK (op_index >= 0),
    event_index         smallint     NOT NULL CHECK (event_index >= 0),

    -- The emitting DeFindex vault contract C-strkey.
    contract_id         text         NOT NULL,

    event_kind          text         NOT NULL CHECK (event_kind IN (
        'rescue', 'paused', 'unpaused',
        'nreceiver', 'nmanager', 'nemanager', 'rbmanager'
    )),

    caller              text,
    strategy            text,
    new_address         text,

    -- rescue's amount_withdrawn (i128; NUMERIC per ADR-0003).
    amount              numeric      CHECK (amount >= 0),

    derive_generation   bigint       NOT NULL DEFAULT 0,
    ingested_at         timestamptz  NOT NULL DEFAULT now(),

    -- Per-kind shape: a row missing a field its topic always carries is
    -- a decoder bug, and must fail loudly rather than serve a hole.
    CONSTRAINT defindex_admin_events_amount_kind
        CHECK ((event_kind = 'rescue') = (amount IS NOT NULL)),
    CONSTRAINT defindex_admin_events_strategy_kind
        CHECK ((event_kind IN ('rescue', 'paused', 'unpaused')) = (strategy IS NOT NULL)),
    CONSTRAINT defindex_admin_events_new_address_kind
        CHECK ((event_kind IN ('nreceiver', 'nmanager', 'nemanager', 'rbmanager'))
               = (new_address IS NOT NULL)),
    CONSTRAINT defindex_admin_events_caller_kind
        CHECK ((event_kind IN ('rescue', 'paused', 'unpaused', 'nreceiver'))
               = (caller IS NOT NULL)),

    -- PK includes ledger_close_time (TimescaleDB TS103); event_index
    -- discriminates same-op sibling events.
    PRIMARY KEY (ledger_close_time, contract_id, ledger, tx_hash,
                 op_index, event_index)
);

COMMENT ON TABLE defindex_admin_events IS
    'DeFindex vault admin events (rescue, paused, unpaused, nreceiver, '
    'nmanager, nemanager, rbmanager): one row per event. Hypertable on '
    'ledger_close_time. See internal/sources/defindex/README.md.';
COMMENT ON COLUMN defindex_admin_events.strategy IS
    'Strategy contract C-strkey (rescue, paused, unpaused).';
COMMENT ON COLUMN defindex_admin_events.new_address IS
    'The newly assigned role holder: fee receiver (nreceiver), manager '
    '(nmanager), emergency manager (nemanager) or rebalance manager (rbmanager).';
COMMENT ON COLUMN defindex_admin_events.amount IS
    'rescue only: amount_withdrawn from the strategy, in base units of '
    'the strategy''s underlying asset.';

SELECT create_hypertable(
    'defindex_admin_events',
    'ledger_close_time',
    chunk_time_interval => INTERVAL '30 days',
    if_not_exists       => TRUE
);

CREATE INDEX defindex_admin_events_tx_hash_idx
    ON defindex_admin_events (tx_hash, ledger_close_time DESC);

CREATE INDEX defindex_admin_events_contract_ts_idx
    ON defindex_admin_events (contract_id, ledger_close_time DESC);

ALTER TABLE defindex_admin_events SET (
    timescaledb.compress,
    timescaledb.compress_segmentby = 'contract_id',
    timescaledb.compress_orderby   = 'ledger_close_time DESC, ledger DESC'
);

SELECT add_compression_policy(
    'defindex_admin_events',
    INTERVAL '30 days',
    if_not_exists => TRUE
);

COMMIT;
