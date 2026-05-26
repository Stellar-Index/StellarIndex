-- 0042 up — `comet_liquidity` hypertable (#26).
--
-- One row per observed (POOL, join_pool | exit_pool | deposit |
-- withdraw) Comet event. These are the four liquidity-mutating
-- events the Soroban port of Balancer-v1 emits alongside the
-- already-handled (POOL, swap) trade event:
--
--   join_pool — multi-token LP add; pool reserves grow.
--               One event per token, so an N-token join produces
--               N rows that share (ledger, tx_hash) — group by
--               those for the logical join.
--   exit_pool — multi-token LP remove; symmetric with join_pool.
--   deposit   — single-asset LP add (one-sided liquidity).
--   withdraw  — single-asset LP remove. Also carries
--               `pool_amount_in` (the BPT-share token count burned
--               in exchange for the underlying); the other three
--               kinds have a NULL pool_amount_in here.
--
-- NOT covered by this table (do NOT add them in a follow-up unless
-- the contract starts emitting events for them):
--
--   - bind / rebind / unbind / finalize — the Soroban Comet port
--     does not implement these functions; the pool's token+weight
--     set is fixed at init() and no events exist.
--   - set_swap_fee / set_public_swap — neither function exists in
--     the Soroban port.
--   - set_controller — exists, but the Soroban port does not
--     publish an event for it.
--   - gulp — exists (absorbs tokens sent directly to the contract),
--     does not publish an event.
--   - BPT (pool-share) `transfer` — emitted via the SEP-41 standard
--     token-event surface, NOT the POOL namespace. Already claimed
--     by internal/sources/sep41_supply when the pool is in scope;
--     re-decoding it here would double-count.
--
-- Storage shape: per-protocol table, same decision taken for
-- cctp_events (0038) / rozo_events (0039) — operator-confirmed
-- 2026-05-22. Comet has no published prices, so these rows never
-- reach the trades hypertable or VWAP.
--
-- Identity: (contract_id, ledger, tx_hash, op_index, event_kind,
-- token). Multi-token joins emit one event per token from the same
-- (ledger, tx_hash, op_index), and the contract has been observed
-- to fold deposit + withdraw onto the same op as well — so `token`
-- drags into the unique key to keep each per-token row distinct.
-- `event_kind` also drags in so a deposit (single-asset) on the
-- same op as a join_pool (multi-token) tail (a contract upgrade
-- might do this) doesn't collide. ledger_close_time drags in
-- because TimescaleDB requires the partition column there (same as
-- cctp_events / rozo_events / sep41_supply_events / soroban_events).
--
-- Retention: NONE — granular-coverage mission keeps liquidity
-- history forever.
--
-- Historical fill: live ingest from rc.X onwards writes here
-- directly. The pre-rc.X back-window will be filled from
-- `soroban_events` (migration 0041) once the operator schedules a
-- per-source backfill — query shape:
--
--   INSERT INTO comet_liquidity (...)
--   SELECT ... FROM soroban_events
--    WHERE topic_0_sym = 'POOL'
--      AND topic_1_xdr IN (<scval-encoded symbols>)
--      AND ledger_close_time BETWEEN <pre-rc.X cut-off> AND now();
--
-- — milliseconds-to-minutes instead of hours of MinIO re-walk.

BEGIN;

CREATE TABLE comet_liquidity (
    -- Emitting pool contract C-strkey. Operators filter by this
    -- to scope to Blend's backstop pool only
    -- (`CAS3FL6TLZKDGGSISDBWGGPXT3NRR4DYTZD7YOD3HMYO6LTJUVGRVEAM`)
    -- or to a broader set if future Comet deployments emerge.
    contract_id        text         NOT NULL,

    -- Soroban event identity.
    ledger             integer      NOT NULL CHECK (ledger >= 0),
    ledger_close_time  timestamptz  NOT NULL,
    tx_hash            char(64)     NOT NULL,
    op_index           integer      NOT NULL CHECK (op_index >= 0),

    -- Which of the four liquidity-mutating Comet events this row
    -- represents. Pinned to the LiquidityKind constants in
    -- internal/sources/comet/events.go.
    event_kind         text         NOT NULL CHECK (event_kind IN (
        'join_pool', 'exit_pool', 'deposit', 'withdraw')),

    -- Convenience derived from event_kind:
    --   join_pool / deposit  → 'add'    (pool reserves grow)
    --   exit_pool / withdraw → 'remove' (pool reserves shrink)
    -- Stored explicitly so dashboards can SUM by direction without
    -- re-encoding the kind mapping in SQL.
    direction          text         NOT NULL CHECK (direction IN ('add', 'remove')),

    -- The address that initiated the liquidity event (`caller` in
    -- every variant's body). Usually the LP user; can be a router
    -- contract for aggregator-routed liquidity.
    caller             text         NOT NULL,

    -- The token that moved (`token_in` for join/deposit,
    -- `token_out` for exit/withdraw). Stellar Address strkey.
    token              text         NOT NULL,

    -- The underlying amount that moved (`token_amount_in` for
    -- join/deposit, `token_amount_out` for exit/withdraw).
    -- NUMERIC per ADR-0003 (i128 amounts never truncate to int64).
    -- > 0 — the decoder rejects zero/negative amounts upstream.
    amount             numeric      NOT NULL CHECK (amount > 0),

    -- withdraw-only: the count of BPT (pool-share) tokens burned in
    -- exchange for the underlying withdrawn. NULL on the other
    -- three kinds. Surfaced for downstream reserve-tracking and
    -- BPT-supply derivation; the SEP-41 supply observer already
    -- tracks the canonical BPT supply via the standard `burn` /
    -- `mint` token events, but pairing the burn with the underlying
    -- withdrawn is unique to this row.
    pool_amount_in     numeric      CHECK (pool_amount_in IS NULL OR pool_amount_in > 0),

    ingested_at        timestamptz  NOT NULL DEFAULT now(),

    -- PK includes ledger_close_time (TimescaleDB partition column
    -- requirement: TS103). `token` is in the key because a
    -- multi-token join_pool emits one event per token from the
    -- same (ledger, tx_hash, op_index) — without `token` those
    -- would collide. `event_kind` is in the key because two
    -- different kinds can in principle land on the same op
    -- (a contract upgrade could fold deposit+join_pool together).
    PRIMARY KEY (ledger_close_time, contract_id, ledger, tx_hash,
                 op_index, event_kind, token)
);

COMMENT ON TABLE comet_liquidity IS
    'Per-event Comet POOL liquidity mutations (join_pool / exit_pool / '
    'deposit / withdraw). Comet has no published price; these rows '
    'never contribute to VWAP. Hypertable on ledger_close_time. '
    'See #26 + internal/sources/comet/README.md.';
COMMENT ON COLUMN comet_liquidity.pool_amount_in IS
    'withdraw-only: count of BPT (pool-share) tokens burned for the '
    'underlying withdrawn. NULL on join_pool / exit_pool / deposit.';
COMMENT ON COLUMN comet_liquidity.direction IS
    'Add/remove polarity derived from event_kind; stored explicitly '
    'so dashboards can SUM(amount) WHERE direction = ''add'' '
    'without re-encoding the kind mapping in SQL.';

SELECT create_hypertable(
    'comet_liquidity',
    'ledger_close_time',
    chunk_time_interval => INTERVAL '7 days',
    if_not_exists       => TRUE
);

-- Per-contract walk ("every liquidity event from this pool, newest
-- first") — operators monitoring a specific pool's LP activity.
CREATE INDEX comet_liquidity_contract_time_idx
    ON comet_liquidity (contract_id, ledger_close_time DESC);

-- Per-kind cross-contract scan ("recent withdraw flow across all
-- Comet pools") — surfaces a sudden LP exit-burst.
CREATE INDEX comet_liquidity_kind_time_idx
    ON comet_liquidity (event_kind, ledger_close_time DESC);

-- Per-token walk ("every Comet liquidity event for this asset") —
-- powers the per-asset depth-flow chart on the explorer.
CREATE INDEX comet_liquidity_token_time_idx
    ON comet_liquidity (token, ledger_close_time DESC);

-- Compression after 7 days. Segment by (contract_id, event_kind)
-- because operator dashboards group by both; order by
-- ledger_close_time DESC for newest-first per-segment reads.
ALTER TABLE comet_liquidity SET (
    timescaledb.compress,
    timescaledb.compress_segmentby = 'contract_id, event_kind',
    timescaledb.compress_orderby   = 'ledger_close_time DESC, ledger DESC'
);

SELECT add_compression_policy(
    'comet_liquidity',
    INTERVAL '7 days',
    if_not_exists => TRUE
);

COMMIT;
