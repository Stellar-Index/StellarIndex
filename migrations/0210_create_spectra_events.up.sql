-- 0210 up — `spectra_events` hypertable and `spectra_markets` table.
--
-- Spectra (docs/protocols/spectra.md) splits an interest-bearing token
-- (IBT) into a principal token (PT) and a yield token (YT) per market.
-- The factory announces each market (`pt_deployed`), the new PT
-- announces its YT (`yt_deployed`) and the registry lists the PT
-- (`pt_added`); all three land in the same ledger on every market seen
-- so far. Every emitter is gated by contract identity (ADR-0035): the
-- `role` column records which gated role emitted the row, and the CHECK
-- below ties each kind to the one role allowed to emit it.
--
-- spectra_events: one row per decoded event of these kinds.
--
--   pt_deployed       factory   market_pt, caller (deployer), ibt, duration_s
--   yt_deployed       pt        market_pt (= the PT), yt
--   pt_added          registry  market_pt
--   pt_minted         pt        market_pt, caller, receiver, shares
--   redeem            pt        market_pt, owner, receiver, shares
--   yield_updated     pt        market_pt, owner (the user), yield_in_ibt
--   transfer          pt | yt   market_pt, caller (from), receiver (to), amount
--   wrap              ibt       caller, receiver, shares, vault_shares
--   unwrap            ibt       caller, receiver, owner, shares, vault_shares
--   deposit/withdraw  ibt       caller, receiver, owner, assets, shares
--   order_registered  order_engine  maker, order_id, amount (making amount)
--   order_filled      order_engine  order_id, amount (actual making amount)
--   order_cancelled   order_engine  maker, order_id
--
-- Every other optional column is NULL for that kind; the two kind
-- CHECKs enforce exactly the listed set. PT/YT `mint`/`burn` are NOT
-- rows: `pt_minted.shares` and the PT and YT `mint.amount` are the same
-- quantity in the same transaction, so writing them would count one
-- deposit three times. PT/YT `transfer` IS a row: nothing else records
-- those tokens' movements.
--
-- spectra_markets: one row per PT, merged column by column from the
-- three discovery kinds by the store's event writer, so a replay of
-- spectra_events refills it. A column stays NULL until its event is
-- seen; the six pt_deployed columns arrive together or not at all.
-- Maturity is not stored: it is inferred as deployed_at + duration_s
-- and still unconfirmed against the operator's published dates.
--
-- Amounts are NUMERIC per ADR-0003 (raw i128, never scaled). An IBT is
-- a market attribute only; it is never admitted to the gate through
-- this table.
--
-- Identity: (ledger_close_time, contract_id, ledger, tx_hash, op_index,
-- event_index), as in 0157. ledger_close_time leads because TimescaleDB
-- requires the partition column in the primary key.
--
-- Retention: NONE on either table. Compression after 30 days is the
-- only ageing policy on spectra_events.

BEGIN;

CREATE TABLE spectra_events (
    contract_id        text         NOT NULL,

    ledger             integer      NOT NULL CHECK (ledger >= 0),
    ledger_close_time  timestamptz  NOT NULL,
    tx_hash            char(64)     NOT NULL,
    op_index           integer      NOT NULL CHECK (op_index >= 0),
    event_index        integer      NOT NULL CHECK (event_index >= 0),

    event_kind         text         NOT NULL CHECK (event_kind IN (
        'pt_deployed', 'yt_deployed', 'pt_added',
        'pt_minted', 'redeem', 'yield_updated', 'transfer',
        'wrap', 'unwrap', 'deposit', 'withdraw',
        'order_registered', 'order_filled', 'order_cancelled')),
    role               text         NOT NULL CHECK (role IN (
        'factory', 'registry', 'pt', 'yt', 'ibt', 'order_engine')),

    -- The PT that identifies the market. NULL on IBT and order-engine
    -- kinds: an IBT can back several markets and an order event does
    -- not name one.
    market_pt          text,

    caller             text,
    receiver           text,
    owner              text,
    maker              text,
    order_id           char(64)     CHECK (order_id ~ '^[0-9a-f]{64}$'),
    ibt                text,
    yt                 text,
    duration_s         bigint       CHECK (duration_s >= 0),

    shares             numeric      CHECK (shares >= 0),
    vault_shares       numeric      CHECK (vault_shares >= 0),
    assets             numeric      CHECK (assets >= 0),
    amount             numeric      CHECK (amount >= 0),
    -- Unsigned in no published source, so no sign CHECK.
    yield_in_ibt       numeric,

    -- Generation guard (migration 0110 convention): a re-derive lands in
    -- place when its generation is >= the stored one; a live gen-0
    -- replay can never revert it.
    derive_generation  bigint       NOT NULL DEFAULT 0,

    ingested_at        timestamptz  NOT NULL DEFAULT now(),

    CONSTRAINT spectra_events_kind_columns CHECK (
        CASE event_kind
            WHEN 'pt_deployed'      THEN role = 'factory' AND market_pt IS NOT NULL
                                     AND caller IS NOT NULL AND ibt IS NOT NULL
                                     AND duration_s IS NOT NULL
            WHEN 'yt_deployed'      THEN role = 'pt' AND market_pt IS NOT NULL AND yt IS NOT NULL
            WHEN 'pt_added'         THEN role = 'registry' AND market_pt IS NOT NULL
            WHEN 'pt_minted'        THEN role = 'pt' AND market_pt IS NOT NULL
                                     AND caller IS NOT NULL AND receiver IS NOT NULL
                                     AND shares IS NOT NULL
            WHEN 'redeem'           THEN role = 'pt' AND market_pt IS NOT NULL
                                     AND owner IS NOT NULL AND receiver IS NOT NULL
                                     AND shares IS NOT NULL
            WHEN 'yield_updated'    THEN role = 'pt' AND market_pt IS NOT NULL
                                     AND owner IS NOT NULL AND yield_in_ibt IS NOT NULL
            WHEN 'transfer'         THEN role IN ('pt', 'yt') AND market_pt IS NOT NULL
                                     AND caller IS NOT NULL AND receiver IS NOT NULL
                                     AND amount IS NOT NULL
            WHEN 'wrap'             THEN role = 'ibt'
                                     AND caller IS NOT NULL AND receiver IS NOT NULL
                                     AND shares IS NOT NULL AND vault_shares IS NOT NULL
            WHEN 'unwrap'           THEN role = 'ibt'
                                     AND caller IS NOT NULL AND receiver IS NOT NULL
                                     AND owner IS NOT NULL
                                     AND shares IS NOT NULL AND vault_shares IS NOT NULL
            WHEN 'order_registered' THEN role = 'order_engine'
                                     AND maker IS NOT NULL AND order_id IS NOT NULL
                                     AND amount IS NOT NULL
            WHEN 'order_filled'     THEN role = 'order_engine'
                                     AND order_id IS NOT NULL AND amount IS NOT NULL
            WHEN 'order_cancelled'  THEN role = 'order_engine'
                                     AND maker IS NOT NULL AND order_id IS NOT NULL
            WHEN 'deposit'          THEN role = 'ibt'
                                     AND caller IS NOT NULL AND receiver IS NOT NULL
                                     AND owner IS NOT NULL
                                     AND assets IS NOT NULL AND shares IS NOT NULL
            WHEN 'withdraw'         THEN role = 'ibt'
                                     AND caller IS NOT NULL AND receiver IS NOT NULL
                                     AND owner IS NOT NULL
                                     AND assets IS NOT NULL AND shares IS NOT NULL
            ELSE FALSE
        END
    ),
    -- With the CHECK above, the count pins every column a kind does not
    -- list to NULL.
    CONSTRAINT spectra_events_kind_column_count CHECK (
        num_nonnulls(market_pt, caller, receiver, owner, maker, order_id,
                     ibt, yt, duration_s,
                     shares, vault_shares, assets, amount, yield_in_ibt)
        = CASE event_kind
              WHEN 'pt_deployed'      THEN 4
              WHEN 'yt_deployed'      THEN 2
              WHEN 'pt_added'         THEN 1
              WHEN 'pt_minted'        THEN 4
              WHEN 'redeem'           THEN 4
              WHEN 'yield_updated'    THEN 3
              WHEN 'transfer'         THEN 4
              WHEN 'wrap'             THEN 4
              WHEN 'unwrap'           THEN 5
              WHEN 'order_registered' THEN 3
              WHEN 'order_filled'     THEN 2
              WHEN 'order_cancelled'  THEN 2
              WHEN 'deposit'          THEN 5
              WHEN 'withdraw'         THEN 5
              ELSE -1
          END
    ),
    -- A PT's own events always belong to its own market.
    CONSTRAINT spectra_events_pt_market CHECK (role <> 'pt' OR market_pt = contract_id),

    PRIMARY KEY (ledger_close_time, contract_id, ledger, tx_hash,
                 op_index, event_index)
);

COMMENT ON TABLE spectra_events IS
    'Per-event Spectra activity (market discovery, PT mint/redeem/yield, '
    'PT/YT transfers, IBT wrapper flows, limit orders) from the gated '
    'pubnet contracts. Amounts are raw i128, unscaled. PT/YT mint and burn '
    'are deliberately not rows: they restate pt_minted / redeem.';

SELECT create_hypertable(
    'spectra_events',
    'ledger_close_time',
    chunk_time_interval => INTERVAL '30 days',
    if_not_exists       => TRUE
);

CREATE INDEX spectra_events_contract_ts_idx
    ON spectra_events (contract_id, ledger_close_time DESC);

CREATE INDEX spectra_events_market_ts_idx
    ON spectra_events (market_pt, ledger_close_time DESC)
    WHERE market_pt IS NOT NULL;

CREATE INDEX spectra_events_kind_ts_idx
    ON spectra_events (event_kind, ledger_close_time DESC);

-- Same-tx correlation: a fill is priced only by the PT/IBT transfers
-- beside it, and pt_deployed / yt_deployed / pt_added share one tx.
CREATE INDEX spectra_events_tx_hash_idx
    ON spectra_events (tx_hash, ledger_close_time DESC);

ALTER TABLE spectra_events SET (
    timescaledb.compress,
    timescaledb.compress_segmentby = 'contract_id, event_kind',
    timescaledb.compress_orderby   = 'ledger_close_time DESC, ledger DESC'
);

SELECT add_compression_policy(
    'spectra_events',
    INTERVAL '30 days',
    if_not_exists => TRUE
);

CREATE TABLE spectra_markets (
    pt               text         NOT NULL PRIMARY KEY,
    -- From yt_deployed.
    yt               text,
    -- From pt_deployed, all six together.
    ibt              text,
    factory_id       text,
    deployer         text,
    duration_s       bigint       CHECK (duration_s >= 0),
    creation_ledger  bigint       CHECK (creation_ledger >= 0),
    deployed_at      timestamptz,
    -- From pt_added: the market is listed in the registry.
    listed_ledger    bigint       CHECK (listed_ledger >= 0),
    updated_at       timestamptz  NOT NULL DEFAULT now(),

    CONSTRAINT spectra_markets_deploy_columns CHECK (
        num_nulls(ibt, factory_id, deployer, duration_s, creation_ledger, deployed_at) IN (0, 6)
    )
);

COMMENT ON TABLE spectra_markets IS
    'One row per Spectra PT market, merged column by column from the '
    'pt_deployed / yt_deployed / pt_added rows of spectra_events by the '
    'same write, so a spectra_events replay refills it. NULL = not yet seen.';

COMMIT;
