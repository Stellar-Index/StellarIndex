-- 0127 up — `soroswap_liquidity` hypertable.
--
-- One row per observed Soroswap pair-contract `deposit` | `withdraw`
-- event — the two liquidity-mutating events a Soroswap pair emits
-- alongside the already-projected `swap` (→ trades) and `skim`
-- (→ soroswap_skim_events). Until audit 2026-08-03 these were
-- classified + Matched but dropped at dispatcher_adapter.go with no
-- table — the raw events sat in the ClickHouse lake unserved,
-- violating the every-event mission. This table + the decoder arm
-- (internal/sources/soroswap/decode.go decodeLiquidity) close that gap.
--
-- Wire shape (verified against mainnet lake bodies, ledgers
-- 62.94M / 63.18M): deposit and withdraw share ONE #[contracttype]
-- struct →  ScvMap with field-name Symbol keys:
--
--   { amount_0: i128, amount_1: i128, liquidity: i128,
--     new_reserve_0: i128, new_reserve_1: i128, to: Address }
--
--   amount_0 / amount_1  — the token0 / token1 amounts added
--                          (deposit) or removed (withdraw).
--   liquidity            — LP-share tokens minted (deposit) /
--                          burned (withdraw). The share token is the
--                          pair contract itself (SEP-41); its supply
--                          is tracked separately by sep41_supply via
--                          the standard mint/burn token events — this
--                          column is the per-event delta, not a supply.
--   new_reserve_0 / _1   — post-state pool reserves after the event.
--   to                   — the LP provider address (`provider`).
--
-- token_0 / token_1 are NOT in the event body — they are a property
-- of the pair, resolved from the factory new_pair registry at decode
-- time and denormalised here for query convenience. The columns are
-- NULLABLE only for defence-in-depth: a deposit/withdraw is recognised
-- (and a row written) ONLY for a pair already in the seeded/factory
-- registry (dispatcher_adapter.go Matches gates on pairTokens), and a
-- registered pair always carries both token addresses — so in practice
-- both are non-NULL. An UNSEEDED pair (mid-history-start race, or a
-- window whose new_pair event is before the replay -from) fails CLOSED
-- into an ADR-0033 recognition gap: the LP events are NOT written with
-- NULL tokens, they are dropped-and-visible exactly like the swap/trade
-- path — never silently attributed to a foreign contract. Filling that
-- gap is a matter of re-deriving from a -from at/before the pair's
-- new_pair event, not of resolving NULLs after the fact.
--
-- Storage shape: per-protocol table, same decision as comet_liquidity
-- (0042) / phoenix_liquidity (0044). Soroswap deposit/withdraw carry
-- no published price; these rows never reach trades or VWAP.
--
-- Identity: (pair, ledger, tx_hash, op_index, event_index, action).
-- event_index drags in because a router zap could emit >1 liquidity
-- event on one pair in one op (comet learned this in 0059); action
-- drags in so a deposit + withdraw folded onto one op (a future
-- upgrade / rebalance flow) doesn't collide. ledger_close_time drags
-- in because TimescaleDB requires the partition column in every unique
-- index on a hypertable (TS103).
--
-- derive_generation: the INV-3 generation-guarded corrective-upsert
-- column (migration 0110), same as trades / soroswap_skim_events — a
-- corrected projector re-derive lands in place when its generation is
-- >= the stored one; a live gen-0 replay can never revert it.
--
-- Retention: NONE — granular-coverage mission keeps LP history forever.
--
-- Historical fill: live ingest writes here directly from deploy; the
-- pre-deploy back-window is filled by `stellarindex-ops
-- projector-replay -source soroswap -from <ledger>` re-deriving
-- deposit/withdraw straight from the ClickHouse lake (ADR-0034) — no
-- MinIO re-walk.

BEGIN;

CREATE TABLE soroswap_liquidity (
    -- Emitting pair contract C-strkey.
    pair               text         NOT NULL,

    -- Soroban event identity.
    ledger             integer      NOT NULL CHECK (ledger >= 0),
    ledger_close_time  timestamptz  NOT NULL,
    tx_hash            bytea        NOT NULL, -- 32-byte raw hash
    op_index           smallint     NOT NULL CHECK (op_index >= 0),
    event_index        smallint     NOT NULL CHECK (event_index >= 0),

    -- Which liquidity-mutating event this row represents.
    action             text         NOT NULL CHECK (action IN ('deposit', 'withdraw')),

    -- Add/remove polarity derived from action (deposit → add,
    -- withdraw → remove). Stored explicitly so dashboards can SUM by
    -- direction without re-encoding the mapping in SQL (comet 0042).
    direction          text         NOT NULL CHECK (direction IN ('add', 'remove')),

    -- The LP provider — the event body's `to` field.
    provider           text         NOT NULL,

    -- Pair token identities, resolved from the factory new_pair
    -- registry (the deposit/withdraw body carries only amounts).
    -- Nullable for defence-in-depth (see header): a row exists only for
    -- a seeded/registered pair, which always carries both tokens, so
    -- these are non-NULL in practice; an unseeded pair is a recognition
    -- gap (no row), not a NULL-token row.
    token_0            text,
    token_1            text,

    -- Per-token amounts moved + LP shares + post-state reserves.
    -- NUMERIC per ADR-0003 (i128 never truncates to int64). >= 0 —
    -- the decoder rejects nothing but the values are non-negative
    -- amounts on-chain.
    amount_0           numeric      NOT NULL CHECK (amount_0 >= 0),
    amount_1           numeric      NOT NULL CHECK (amount_1 >= 0),
    liquidity          numeric      NOT NULL CHECK (liquidity >= 0),
    new_reserve_0      numeric      NOT NULL CHECK (new_reserve_0 >= 0),
    new_reserve_1      numeric      NOT NULL CHECK (new_reserve_1 >= 0),

    derive_generation  integer      NOT NULL DEFAULT 0,
    ingested_at        timestamptz  NOT NULL DEFAULT now(),

    -- PK includes ledger_close_time (TimescaleDB TS103). See header
    -- for why event_index + action drag in.
    PRIMARY KEY (ledger_close_time, pair, ledger, tx_hash, op_index,
                 event_index, action)
);

COMMENT ON TABLE soroswap_liquidity IS
    'Per-event Soroswap pair deposit / withdraw (LP add / remove) '
    'rows. Soroswap deposit/withdraw carry no published price; these '
    'rows never contribute to VWAP. Hypertable on ledger_close_time. '
    'See internal/sources/soroswap/decode.go decodeLiquidity.';
COMMENT ON COLUMN soroswap_liquidity.liquidity IS
    'LP-share tokens minted (deposit) / burned (withdraw) for this '
    'event. The share token is the pair contract itself; its supply '
    'is tracked by sep41_supply via standard mint/burn token events.';
COMMENT ON COLUMN soroswap_liquidity.token_0 IS
    'Resolved from the factory new_pair registry, not the event body; '
    'NULL when the pair mapping was unseeded at decode time '
    '(resolvable downstream via soroswap_pairs).';

SELECT create_hypertable(
    'soroswap_liquidity',
    'ledger_close_time',
    chunk_time_interval => INTERVAL '7 days',
    if_not_exists       => TRUE
);

-- Per-pair walk ("every LP event for this pool, newest first").
CREATE INDEX soroswap_liquidity_pair_time_idx
    ON soroswap_liquidity (pair, ledger_close_time DESC);

-- Per-provider walk ("an LP's positions across pools").
CREATE INDEX soroswap_liquidity_provider_time_idx
    ON soroswap_liquidity (provider, ledger_close_time DESC);

-- Cross-pair per-action scan ("recent withdraw flow across Soroswap").
CREATE INDEX soroswap_liquidity_action_time_idx
    ON soroswap_liquidity (action, ledger_close_time DESC);

ALTER TABLE soroswap_liquidity SET (
    timescaledb.compress,
    timescaledb.compress_segmentby = 'pair, action',
    timescaledb.compress_orderby   = 'ledger_close_time DESC, ledger DESC'
);

SELECT add_compression_policy(
    'soroswap_liquidity',
    INTERVAL '7 days',
    if_not_exists => TRUE
);

COMMIT;
