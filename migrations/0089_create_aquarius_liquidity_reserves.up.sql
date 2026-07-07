-- 0089 up — Aquarius liquidity + reserves event hypertables.
--
-- Aquarius is the highest-volume Soroban AMM but until now the
-- decoder (internal/sources/aquarius) only emitted the `trade` event
-- to the `trades` hypertable. The pool contracts also emit a large
-- liquidity/reserves surface that was classified-but-dropped:
--
--   update_reserves    — per-pool POST-STATE reserves (one i128 per
--                        pool token). By FAR the densest stream
--                        (fires on every state-changing pool op) —
--                        the first real Aquarius TVL / liquidity-depth
--                        signal. Body: Vec<i128> of reserves; NO token
--                        addresses in topics (positional, in the
--                        pool's canonical token order).
--   deposit_liquidity  — LP add. Topics: (Symbol, token_0..token_{n-1});
--                        body: Vec<i128> = (amount_0..amount_{n-1},
--                        share_amount). The trailing element is the LP
--                        shares MINTED.
--   withdraw_liquidity — LP remove. Same wire shape as deposit; the
--                        trailing body element is the LP shares BURNED.
--
-- Aquarius has N-token pools (volatile = 2, stableswap = 2/3/4).
-- Verified against the r1 lake 2026-07-06: deposit/withdraw carry
-- topic_count 3/4/5 (2/3/4 tokens) and update_reserves body length
-- tracks the pool's token count. Both tables therefore FAN OUT one row
-- per token position (token_index) rather than hard-coding a 2-token
-- (a/b) shape that would silently drop the 3-4-token stableswap events
-- — same one-row-per-token decision as comet_liquidity (0042).
--
-- Storage shape: per-protocol tables, same decision taken for
-- comet_liquidity (0042) / phoenix_liquidity (0044) / cctp_events
-- (0038). Aquarius pools have no independently-published price, so
-- these rows never reach the trades hypertable or VWAP — this is
-- ADDITIVE analytics coverage, orthogonal to pricing.
--
-- Contract-identity gate (ADR-0035/0040, CS-026): the decoder claims
-- these events ONLY when emitted by a REGISTERED Aquarius pool (the
-- same router-anchored gate the `trade` decoder already uses) — a
-- look-alike emitting the bare topic symbols cannot inject fabricated
-- reserves.
--
-- Retention: NONE — the granular-coverage mission keeps liquidity /
-- reserve history forever.
--
-- Historical fill: live ingest writes here from deploy onward; the
-- back-window is re-derived from the raw lake with
--   stellarindex-ops projector-replay -source aquarius -from <genesis>
-- (re-deriving trades is a no-op via ON CONFLICT DO NOTHING; only the
-- new reserves/liquidity rows land). Run it under
-- /usr/local/sbin/run-heavy-job.sh (CLAUDE.md heavy-job doctrine).

BEGIN;

-- ─── aquarius_reserves ──────────────────────────────────────────
--
-- One row per (pool, update_reserves event, token position). The
-- reserve at token_index i is the pool's POST-STATE reserve for the
-- token at position i in the pool's canonical token order (the same
-- order the pool's deposit/withdraw/trade topics list the tokens).
-- update_reserves carries NO token address — the position is the
-- only identity — so downstream correlates token_index → token
-- address via a recent deposit_liquidity / trade for the same pool.
CREATE TABLE aquarius_reserves (
    -- Emitting pool contract C-strkey. Operators filter by this to
    -- scope to one pool's reserve/TVL time series.
    contract_id        text         NOT NULL,

    -- Soroban event identity.
    ledger             integer      NOT NULL CHECK (ledger >= 0),
    ledger_close_time  timestamptz  NOT NULL,
    tx_hash            text         NOT NULL,
    op_index           integer      NOT NULL CHECK (op_index >= 0),
    -- Per-event discriminator within the op (an op can emit several
    -- update_reserves events — one per pool touched). Same role as the
    -- comet_liquidity.event_index / phoenix event_index (F-1324).
    event_index        integer      NOT NULL CHECK (event_index >= 0),

    -- Position of this reserve in the pool's canonical token order
    -- (0-based). token_index < pool token count.
    token_index        smallint     NOT NULL CHECK (token_index >= 0),

    -- POST-STATE reserve for the token at token_index. NUMERIC per
    -- ADR-0003 (i128 amounts never truncate to int64). A reserve can
    -- transiently be 0 (freshly created / fully drained pool), so the
    -- check is >= 0, not > 0.
    reserve            numeric      NOT NULL CHECK (reserve >= 0),

    ingested_at        timestamptz  NOT NULL DEFAULT now(),

    -- PK includes ledger_close_time (TimescaleDB partition-column
    -- requirement, TS103). token_index drags in so the N fanned rows
    -- of one event stay distinct; event_index so two update_reserves
    -- events in one op don't collide.
    PRIMARY KEY (ledger_close_time, contract_id, ledger, tx_hash,
                 op_index, event_index, token_index)
);

COMMENT ON TABLE aquarius_reserves IS
    'Per-token Aquarius pool POST-STATE reserves (update_reserves event, '
    'fanned one row per token position). The first real Aquarius TVL / '
    'liquidity-depth signal. Aquarius has no published price; these rows '
    'never contribute to VWAP. Hypertable on ledger_close_time. See '
    'internal/sources/aquarius/README.md.';
COMMENT ON COLUMN aquarius_reserves.token_index IS
    'Position of the reserve in the pool''s canonical token order. '
    'update_reserves carries no token address — join a recent '
    'deposit_liquidity / trade for the same pool to resolve the address.';

SELECT create_hypertable(
    'aquarius_reserves',
    'ledger_close_time',
    chunk_time_interval => INTERVAL '7 days',
    if_not_exists       => TRUE
);

-- Per-pool walk ("this pool's reserve/TVL history, newest first").
CREATE INDEX aquarius_reserves_contract_ts_idx
    ON aquarius_reserves (contract_id, ledger_close_time DESC);

ALTER TABLE aquarius_reserves SET (
    timescaledb.compress,
    timescaledb.compress_segmentby = 'contract_id',
    timescaledb.compress_orderby   = 'ledger_close_time DESC, ledger DESC'
);

SELECT add_compression_policy(
    'aquarius_reserves',
    INTERVAL '7 days',
    if_not_exists => TRUE
);

-- ─── aquarius_liquidity ─────────────────────────────────────────
--
-- One row per (pool, deposit/withdraw event, token position). The
-- amount at token_index i is the amount of the pool's token i that
-- was deposited (add) or withdrawn (remove). `shares` (the LP-share
-- amount minted on deposit / burned on withdraw) is a single
-- per-EVENT value, so it is stored ONLY on the canonical token_index
-- = 0 row and NULL on the other positions — this keeps SUM(shares)
-- across the table honest (no N-counting) while pinning the value to a
-- deterministic row. Mirrors the "nullable per-event column" pattern
-- of comet_liquidity.pool_amount_in / phoenix_liquidity.shares_amount.
CREATE TABLE aquarius_liquidity (
    contract_id        text         NOT NULL,

    ledger             integer      NOT NULL CHECK (ledger >= 0),
    ledger_close_time  timestamptz  NOT NULL,
    tx_hash            text         NOT NULL,
    op_index           integer      NOT NULL CHECK (op_index >= 0),
    event_index        integer      NOT NULL CHECK (event_index >= 0),

    -- deposit (LP add; reserves grow) | withdraw (LP remove).
    action             text         NOT NULL CHECK (action IN ('deposit', 'withdraw')),

    -- Position of this token in the pool's canonical token order.
    token_index        smallint     NOT NULL CHECK (token_index >= 0),

    -- The token that moved at token_index (Stellar Address strkey,
    -- from the event topics).
    token              text         NOT NULL,

    -- Amount of `token` deposited / withdrawn. NUMERIC per ADR-0003.
    -- A single leg of a multi-token, imbalanced add/remove can be 0,
    -- so the check is >= 0.
    amount             numeric      NOT NULL CHECK (amount >= 0),

    -- LP-share amount minted (deposit) / burned (withdraw). Single
    -- per-event value: populated on token_index = 0, NULL elsewhere.
    shares             numeric      CHECK (shares IS NULL OR shares >= 0),

    ingested_at        timestamptz  NOT NULL DEFAULT now(),

    -- PK includes ledger_close_time per TS103. token_index keeps the N
    -- fanned rows distinct; action + event_index guard against two
    -- liquidity events colliding on one op.
    PRIMARY KEY (ledger_close_time, contract_id, ledger, tx_hash,
                 op_index, event_index, action, token_index)
);

COMMENT ON TABLE aquarius_liquidity IS
    'Per-token Aquarius pool deposit_liquidity / withdraw_liquidity '
    'events (fanned one row per token position). Aquarius has no '
    'published price; these rows never contribute to VWAP. Hypertable '
    'on ledger_close_time. See internal/sources/aquarius/README.md.';
COMMENT ON COLUMN aquarius_liquidity.shares IS
    'LP-share amount minted (deposit) / burned (withdraw). Per-event, '
    'stored on token_index = 0 only (NULL on other positions) so '
    'SUM(shares) does not N-count a multi-token event.';

SELECT create_hypertable(
    'aquarius_liquidity',
    'ledger_close_time',
    chunk_time_interval => INTERVAL '7 days',
    if_not_exists       => TRUE
);

-- Per-pool walk (dominant query shape).
CREATE INDEX aquarius_liquidity_contract_ts_idx
    ON aquarius_liquidity (contract_id, ledger_close_time DESC);

-- Per-token walk ("every Aquarius LP event for this asset") — powers
-- a per-asset depth-flow chart.
CREATE INDEX aquarius_liquidity_token_ts_idx
    ON aquarius_liquidity (token, ledger_close_time DESC);

-- Cross-pool per-action scan ("recent withdraws across Aquarius").
CREATE INDEX aquarius_liquidity_action_ts_idx
    ON aquarius_liquidity (action, ledger_close_time DESC);

ALTER TABLE aquarius_liquidity SET (
    timescaledb.compress,
    timescaledb.compress_segmentby = 'contract_id, action',
    timescaledb.compress_orderby   = 'ledger_close_time DESC, ledger DESC'
);

SELECT add_compression_policy(
    'aquarius_liquidity',
    INTERVAL '7 days',
    if_not_exists => TRUE
);

COMMIT;
