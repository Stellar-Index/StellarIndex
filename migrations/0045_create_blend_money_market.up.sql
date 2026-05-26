-- 0045 up — Blend money-market hypertables (#25).
--
-- The Blend pool / pool-factory contracts emit 22 distinct event
-- topics. Migration 0009 covered the 3 auction topics
-- (new_auction / fill_auction / delete_auction) — every other topic
-- hit the default-skip branch in internal/sources/blend/decode.go,
-- so historically we captured ~2% of real Blend volume. This
-- migration adds the storage for the other 18 topics, split across
-- three per-purpose tables matching the three downstream consumer
-- shapes:
--
--   blend_positions  — per-user, per-asset, per-pool position deltas
--                      (supply, withdraw, supply_collateral,
--                       withdraw_collateral, borrow, repay,
--                       flash_loan). One row per event; aggregation
--                       into a "current position" is a read-side
--                       roll-up, not a SQL CASE WHEN here. Powers
--                       Freighter V2's asset-detail supply-side
--                       metrics + the in-progress lending pool
--                       explorer surface.
--
--   blend_emissions  — emission accounting + protocol-health events
--                      (gulp, claim, reserve_emission_update,
--                       gulp_emissions, bad_debt, defaulted_debt).
--                      Heterogeneous shapes, so a typed columns +
--                      jsonb attributes split per the cctp_events
--                      (0038) template.
--
--   blend_admin      — admin / pool-config / pool-factory lifecycle
--                      (set_admin, update_pool, queue_set_reserve,
--                       cancel_set_reserve, set_reserve, set_status,
--                       deploy). Same typed-columns + attributes
--                       split — operational state for degraded-
--                       source detection + pool-roster bootstrap.
--
-- Reference: events.rs in blend-contracts-v2
-- (pool/src/events.rs + pool-factory/src/events.rs at commit
-- c19abee5b9be4f49e0cda9057e87d343e5dcc095).
--
-- ── Per ADR-0003 every i128 amount is NUMERIC. ──
-- supply/withdraw/borrow/repay/flash_loan carry two i128 amounts
-- (the underlying-token amount AND the b/d-token amount minted /
-- burnt — both can exceed 2^63). Truncation to int64 is the bug
-- we will catch in review every time. Storage uses NUMERIC; the
-- Go layer uses *big.Int and the JSON wire shape is a string.
--
-- ── Hypertable shape ──
-- All three tables are hypertables on ledger_close_time. Daily
-- chunks consistent with trades / oracle_updates / soroban_events.
-- PK includes ledger_close_time per TimescaleDB TS103 requirement
-- (every unique index on a hypertable must contain the partitioning
-- column). PK identity tuple is per-event identity:
--   (pool, ledger, tx_hash, op_index, event_kind, ledger_close_time)
-- The event_kind field is in the PK as defence in depth — a single
-- (pool, ledger, tx_hash, op_index) tuple is unique on chain, but
-- the kind distinguishes overlapping rows from a split where, e.g.,
-- a single op emits both a supply and a borrow.
--
-- ── Retention ──
-- NONE on any of the three tables. Per the granular-coverage mission,
-- Blend position / emissions / admin history is kept forever.
-- Compression after 7 days, segment-by (pool, event_kind) for
-- dictionary reuse (every supply event from one pool compresses
-- well together).
--
-- ── Index strategy ──
-- Per-table the queries are different:
--
--   blend_positions: dominant query is "every position event for
--     this (pool, user, asset)" — feeds Freighter V2 asset detail.
--     Secondary is the per-pool stream.
--
--   blend_emissions: dominant query is "recent emission flow on
--     this pool" + "every claim for this user".
--
--   blend_admin: dominant query is "recent admin changes" +
--     pool-factory deploy stream for pool enumeration.

BEGIN;

-- ─── blend_positions ───────────────────────────────────────────
-- Per-user, per-asset, per-pool position-changing events. One row
-- per Blend `supply`, `withdraw`, `supply_collateral`,
-- `withdraw_collateral`, `borrow`, `repay`, `flash_loan` event.
--
-- Wire-shape (from pool/src/events.rs):
--   supply / withdraw / supply_collateral / withdraw_collateral:
--     topics: [Symbol("..."), Address(asset), Address(from)]
--     body:   (tokens_amount: i128, b_or_d_token_amount: i128)
--   borrow / repay:
--     topics: [Symbol("..."), Address(asset), Address(from)]
--     body:   (tokens_amount: i128, d_token_amount: i128)
--   flash_loan:
--     topics: [Symbol("flash_loan"), Address(asset), Address(from), Address(contract)]
--     body:   (tokens_out: i128, d_tokens_minted: i128)
--
-- Universal columns + typed columns for the two i128 amounts. The
-- flash_loan `contract` field lands in `counterparty` (the
-- borrowing contract that fronted the flash loan); other event
-- kinds leave `counterparty` NULL.

CREATE TABLE blend_positions (
    -- Pool contract C-strkey (the emitting contract).
    pool               text         NOT NULL,

    -- Per-event identity (Soroban ledger coordinates).
    ledger             integer      NOT NULL CHECK (ledger >= 0),
    tx_hash            char(64)     NOT NULL,
    op_index           integer      NOT NULL CHECK (op_index >= 0),
    ledger_close_time  timestamptz  NOT NULL,

    -- One of the seven money-market event kinds. Strings match the
    -- internal/sources/blend Event* constants.
    event_kind         text         NOT NULL CHECK (event_kind IN (
        'supply', 'withdraw',
        'supply_collateral', 'withdraw_collateral',
        'borrow', 'repay', 'flash_loan'
    )),

    -- (asset, user) from topics 1+2 — universal across all seven
    -- money-market events.
    asset              text         NOT NULL,  -- Address strkey (G or C)
    user_address       text         NOT NULL,  -- Address strkey (G or C)

    -- The two i128 amounts the event publishes. Names match the
    -- contract-side variable names:
    --   token_amount    = tokens_in (supply / repay / supply_collateral)
    --                   = tokens_out (withdraw / borrow / withdraw_collateral / flash_loan)
    --   b_or_d_amount   = b_tokens_minted / b_tokens_burnt (supply* / withdraw*)
    --                   = d_tokens_minted / d_tokens_burnt (borrow / repay / flash_loan)
    -- The decoder writes the amount unmodified — sign / direction
    -- is implicit in event_kind. NUMERIC per ADR-0003.
    token_amount       numeric      NOT NULL,
    b_or_d_amount      numeric      NOT NULL,

    -- For flash_loan: the borrowing contract that fronted the loan.
    -- NULL for every other event kind.
    counterparty       text,

    ingested_at        timestamptz  NOT NULL DEFAULT now(),

    -- ledger_close_time is in the PK to satisfy TimescaleDB's TS103
    -- requirement (partitioning column must appear in every unique
    -- index on a hypertable). event_kind is in the PK as defence in
    -- depth — a single op could in principle emit two different
    -- money-market events (e.g. a transaction that supplies then
    -- borrows in one contract call).
    PRIMARY KEY (pool, ledger, tx_hash, op_index, event_kind, ledger_close_time)
);

COMMENT ON TABLE blend_positions IS
    'Per-event Blend money-market position changes (supply / withdraw '
    '/ supply_collateral / withdraw_collateral / borrow / repay / '
    'flash_loan). Hypertable on ledger_close_time. See #25 + '
    'docs/discovery/dexes-amms/blend.md.';
COMMENT ON COLUMN blend_positions.token_amount IS
    'Underlying token i128 amount (tokens_in or tokens_out — direction '
    'implicit in event_kind). NUMERIC per ADR-0003.';
COMMENT ON COLUMN blend_positions.b_or_d_amount IS
    'b/d-token i128 amount minted / burnt by the event. Supply / withdraw '
    'events touch b_tokens; borrow / repay / flash_loan touch d_tokens.';
COMMENT ON COLUMN blend_positions.counterparty IS
    'flash_loan only: borrowing contract that fronted the loan. NULL otherwise.';

SELECT create_hypertable(
    'blend_positions',
    'ledger_close_time',
    chunk_time_interval => INTERVAL '1 day',
    if_not_exists       => TRUE
);

-- Primary read-side: "show me every position event for this
-- (pool, user, asset)" — Freighter V2 asset-detail.
CREATE INDEX blend_positions_pool_user_asset_idx
    ON blend_positions (pool, user_address, asset, ledger_close_time DESC);

-- Per-pool stream ("recent activity in this Blend pool").
CREATE INDEX blend_positions_pool_ts_idx
    ON blend_positions (pool, ledger_close_time DESC);

-- Per-asset cross-pool scan ("all USDC supply events").
CREATE INDEX blend_positions_asset_kind_ts_idx
    ON blend_positions (asset, event_kind, ledger_close_time DESC);

-- Source-centric replay / debug — walk by ledger.
CREATE INDEX blend_positions_ledger_idx
    ON blend_positions (ledger DESC);

ALTER TABLE blend_positions SET (
    timescaledb.compress,
    timescaledb.compress_segmentby = 'pool, event_kind',
    timescaledb.compress_orderby   = 'ledger_close_time DESC, ledger DESC'
);

SELECT add_compression_policy('blend_positions', INTERVAL '7 days');


-- ─── blend_emissions ───────────────────────────────────────────
-- Emission accounting (gulp / claim / reserve_emission_update /
-- gulp_emissions) + credit-risk events (bad_debt /
-- defaulted_debt). Heterogeneous body shapes; the universal
-- typed columns are amount + asset, with the remainder in jsonb
-- attributes.
--
-- Wire-shape:
--   gulp:                topics [Symbol, Address(asset)]   body i128(token_delta)
--   claim:               topics [Symbol, Address(from)]    body (Vec<u32>, i128)
--   reserve_emission_update: topics [Symbol]               body (u32, u64, u64)
--   gulp_emissions:      topics [Symbol]                   body i128
--   bad_debt:            topics [Symbol, Address(user), Address(asset)]  body i128
--   defaulted_debt:      topics [Symbol, Address(asset)]   body i128

CREATE TABLE blend_emissions (
    pool               text         NOT NULL,

    ledger             integer      NOT NULL CHECK (ledger >= 0),
    tx_hash            char(64)     NOT NULL,
    op_index           integer      NOT NULL CHECK (op_index >= 0),
    ledger_close_time  timestamptz  NOT NULL,

    event_kind         text         NOT NULL CHECK (event_kind IN (
        'gulp', 'claim',
        'reserve_emission_update', 'gulp_emissions',
        'bad_debt', 'defaulted_debt'
    )),

    -- Promoted typed columns. NULL when the event kind doesn't
    -- carry the column.
    --
    --   amount: gulp.token_delta / claim.amount_claimed /
    --           gulp_emissions / bad_debt.d_tokens /
    --           defaulted_debt.d_tokens_burnt — every event's
    --           single (or primary) i128 amount.
    --   asset:  gulp.asset / bad_debt.asset / defaulted_debt.asset —
    --           NULL for claim / reserve_emission_update / gulp_emissions.
    --   user_address: bad_debt.user / claim.from — NULL otherwise.
    amount             numeric,
    asset              text,
    user_address       text,

    -- Event-type-specific remainder. Per type:
    --   reserve_emission_update: {res_token_id: u32, eps: u64, expiration: u64}
    --   claim:                   {reserve_token_ids: [u32, ...]} (amount in `amount`)
    attributes         jsonb        NOT NULL DEFAULT '{}'::jsonb,

    ingested_at        timestamptz  NOT NULL DEFAULT now(),

    PRIMARY KEY (pool, ledger, tx_hash, op_index, event_kind, ledger_close_time)
);

COMMENT ON TABLE blend_emissions IS
    'Per-event Blend emission accounting + credit-risk events. '
    'Hypertable on ledger_close_time. See #25.';
COMMENT ON COLUMN blend_emissions.amount IS
    'Primary i128 amount (gulp.token_delta / claim.amount_claimed / '
    'gulp_emissions / bad_debt.d_tokens / defaulted_debt.d_tokens_burnt). '
    'NUMERIC per ADR-0003.';

SELECT create_hypertable(
    'blend_emissions',
    'ledger_close_time',
    chunk_time_interval => INTERVAL '1 day',
    if_not_exists       => TRUE
);

-- Per-pool stream of emissions activity.
CREATE INDEX blend_emissions_pool_ts_idx
    ON blend_emissions (pool, ledger_close_time DESC);

-- Per-user claim history ("every claim for this address").
CREATE INDEX blend_emissions_user_kind_ts_idx
    ON blend_emissions (user_address, event_kind, ledger_close_time DESC)
    WHERE user_address IS NOT NULL;

-- Per-asset credit-risk feed ("every bad_debt + defaulted_debt for X").
CREATE INDEX blend_emissions_asset_kind_ts_idx
    ON blend_emissions (asset, event_kind, ledger_close_time DESC)
    WHERE asset IS NOT NULL;

ALTER TABLE blend_emissions SET (
    timescaledb.compress,
    timescaledb.compress_segmentby = 'pool, event_kind',
    timescaledb.compress_orderby   = 'ledger_close_time DESC, ledger DESC'
);

SELECT add_compression_policy('blend_emissions', INTERVAL '7 days');


-- ─── blend_admin ───────────────────────────────────────────────
-- Admin / pool-config / pool-factory lifecycle events.
--
-- Wire-shape:
--   set_admin:           topics [Symbol, Address(admin)]   body Address(new_admin)
--   update_pool:         topics [Symbol, Address(admin)]   body (u32, u32, i128)
--   queue_set_reserve:   topics [Symbol, Address(admin)]   body (Address(asset), ReserveConfig)
--   cancel_set_reserve:  topics [Symbol, Address(admin)]   body Address(asset)
--   set_reserve:         topics [Symbol]                   body (Address(asset), u32)
--   set_status:          topics [Symbol] OR [Symbol, Address(admin)]  body u32(status)
--   deploy (factory):    topics [Symbol]                   body Address(pool_address)
--
-- `contract_id` distinguishes pool-emitted events from factory-
-- emitted (deploy). pool / factory share the table because the
-- query patterns overlap ("recent admin activity across Blend").

CREATE TABLE blend_admin (
    -- Emitting contract (pool C-strkey for pool events, pool-factory
    -- C-strkey for `deploy`).
    contract_id        text         NOT NULL,

    ledger             integer      NOT NULL CHECK (ledger >= 0),
    tx_hash            char(64)     NOT NULL,
    op_index           integer      NOT NULL CHECK (op_index >= 0),
    ledger_close_time  timestamptz  NOT NULL,

    event_kind         text         NOT NULL CHECK (event_kind IN (
        'set_admin', 'update_pool',
        'queue_set_reserve', 'cancel_set_reserve', 'set_reserve',
        'set_status', 'deploy'
    )),

    -- Universal typed columns; NULL when the event kind doesn't
    -- carry the column.
    --   admin:       set_admin / update_pool / queue_set_reserve /
    --                cancel_set_reserve / set_status_admin (topic[1])
    --   asset:       queue_set_reserve.asset / cancel_set_reserve.asset /
    --                set_reserve.asset
    --   target:      set_admin.new_admin / deploy.pool_address —
    --                the "addressed entity" of the event
    admin              text,
    asset              text,
    target             text,

    -- Event-type-specific remainder. Per type:
    --   update_pool:       {backstop_take_rate: u32, max_positions: u32, min_collateral: "i128"}
    --   queue_set_reserve: {metadata: {index, decimals, c_factor, ...}} (full ReserveConfig)
    --   set_reserve:       {index: u32}
    --   set_status:        {status: u32, by_admin: bool}
    attributes         jsonb        NOT NULL DEFAULT '{}'::jsonb,

    ingested_at        timestamptz  NOT NULL DEFAULT now(),

    PRIMARY KEY (contract_id, ledger, tx_hash, op_index, event_kind, ledger_close_time)
);

COMMENT ON TABLE blend_admin IS
    'Per-event Blend admin / pool-config / pool-factory lifecycle. '
    'Hypertable on ledger_close_time. The `deploy` event from the '
    'pool-factory drives pool enumeration. See #25.';
COMMENT ON COLUMN blend_admin.target IS
    'set_admin.new_admin / deploy.pool_address — the "addressed entity" of the event.';

SELECT create_hypertable(
    'blend_admin',
    'ledger_close_time',
    chunk_time_interval => INTERVAL '1 day',
    if_not_exists       => TRUE
);

-- Recent admin activity stream.
CREATE INDEX blend_admin_contract_ts_idx
    ON blend_admin (contract_id, ledger_close_time DESC);

-- Per-kind feed ("every deploy event from the factory" / "every
-- update_pool across all pools").
CREATE INDEX blend_admin_kind_ts_idx
    ON blend_admin (event_kind, ledger_close_time DESC);

ALTER TABLE blend_admin SET (
    timescaledb.compress,
    timescaledb.compress_segmentby = 'contract_id, event_kind',
    timescaledb.compress_orderby   = 'ledger_close_time DESC, ledger DESC'
);

SELECT add_compression_policy('blend_admin', INTERVAL '7 days');

COMMIT;
