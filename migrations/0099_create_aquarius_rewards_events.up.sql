-- 0099 up — Aquarius rewards-gauge event surface.
--
-- ROADMAP #89 (2026-07-10 topic census): a read-only ClickHouse-lake
-- topic census against the full gated set (332 pools + router) found
-- 11 real, distinct rewards-gauge topics `classify()` did not
-- recognize — a per-pool liquidity-mining subsystem layered on top of
-- the swap/liquidity surface migration 0089 already covers. None
-- error loudly; they simply never reached a decoder. Lifetime counts
-- (raw `stellar.contract_events`, 2026-07-10):
--
--   pool_state                     339,712  gauge global-state snapshot
--                                            (fires on every reward-
--                                            affecting pool interaction)
--   claim_reward                   263,673  user claims accrued reward
--   set_rewards_config              47,530  admin sets/refreshes the
--                                            reward-rate schedule
--   position_update                 12,403  user's staked-position
--                                            checkpoint
--   deposit (bare)                   7,213  stake deposit into the
--                                            gauge (distinct from
--                                            deposit_liquidity, 0089)
--   claim_fees                       5,056  protocol-fee-share claim
--   rewards_gauge_claim               1,121 gauge-wrapper claim path
--   claim (bare)                       168  distinct from
--                                            claim_protocol_fee
--   rewards_gauge_schedule_reward        64  schedule a future reward
--   set_rewards_state                   25  gauge state toggle
--   rewards_gauge_add                   12  gauge registration
--
-- A twelfth kind, `config_rewards`, is folded in here too: it is the
-- ROUTER-side companion to `set_rewards_config` (real-lake-bytes
-- cross-check: identical amount + expires_at pair in the same tx as
-- the correlated pool's set_rewards_config), not one of the 11
-- pool-scoped topics above and not part of the original 19-topic
-- census (which scanned pool events only), but equally undecoded
-- before this migration and already flagged as a gap in
-- docs/protocols/aquarius.md line 23 — closed in the same pass rather
-- than left as a second, separately-tracked item. 52,722+ lifetime
-- events, 100% emitted by the canonical router.
--
-- Per docs/architecture/contract-schema-evolution.md and the EVERY-
-- event policy (project memory project_every_event_principle), this
-- is decoded by Map-field-name where the body is a Map, and
-- positionally only where the body is a Vec/tuple (the only wire
-- representation Soroban has for tuples) — see
-- internal/sources/aquarius/decode_rewards.go for the per-kind wire
-- shapes, each cited against real r1 lake bytes.
--
-- Storage shape: ONE table with an event_kind discriminator + a
-- handful of universal promoted columns (user_address, amount) +
-- a jsonb `attributes` remainder for kind-specific fields — same
-- decision blend_admin (migration 0045) took for its seven admin
-- event kinds, rather than one table per kind. i128 fields inside
-- jsonb are decimal strings (ADR-0003 — NUMERIC inside jsonb is
-- lossy; a decimal string round-trips at full precision).
--
-- Aquarius has no published price for reward tokens at this layer;
-- these rows are additive analytics, never VWAP inputs — same
-- decision as aquarius_liquidity / aquarius_reserves (0089).
--
-- Retention: NONE — granular-coverage mission keeps this forever.
--
-- Historical fill: live ingest writes here from deploy onward; the
-- back-window is re-derived from the raw lake with
--   stellarindex-ops projector-replay -source aquarius -from 52728375
-- (re-deriving trade/liquidity/reserves rows is a no-op via
-- ON CONFLICT DO NOTHING; only the new rewards rows land). Run under
-- /usr/local/sbin/run-heavy-job.sh (CLAUDE.md heavy-job doctrine).

BEGIN;

CREATE TABLE aquarius_rewards_events (
    -- Emitting contract C-strkey: a registered pool for eleven of the
    -- twelve kinds (every rewards-gauge topic in the census was
    -- observed on a registered pool contract, not a separate
    -- gauge-factory contract), OR the canonical router for
    -- 'config_rewards' (its ROUTER-side companion event, folded into
    -- this table rather than tracked as a second gap — see
    -- decode_rewards.go).
    contract_id        text         NOT NULL,

    ledger             integer      NOT NULL CHECK (ledger >= 0),
    ledger_close_time  timestamptz  NOT NULL,
    tx_hash            text         NOT NULL,
    op_index           integer      NOT NULL CHECK (op_index >= 0),
    -- Per-event discriminator within the op — same role as
    -- aquarius_reserves.event_index / aquarius_liquidity.event_index
    -- (F-1324 class: without this, two rewards events in one op
    -- collide on ON CONFLICT DO NOTHING and one is silently dropped).
    event_index        integer      NOT NULL CHECK (event_index >= 0),

    event_kind         text         NOT NULL CHECK (event_kind IN (
        'pool_state', 'claim_reward', 'set_rewards_config',
        'position_update', 'deposit', 'claim_fees',
        'rewards_gauge_claim', 'claim',
        'rewards_gauge_schedule_reward', 'set_rewards_state',
        'rewards_gauge_add', 'config_rewards'
    )),

    -- Universal typed columns; NULL when the event kind doesn't
    -- carry them. user_address: claim_reward / position_update /
    -- deposit / claim_fees / rewards_gauge_claim / claim (topic
    -- slot). amount: the single dominant i128 amount for user-
    -- action kinds (reward claimed / staked / fee share) — NUMERIC
    -- per ADR-0003.
    user_address       text,
    amount             numeric      CHECK (amount IS NULL OR amount >= 0),

    -- Event-kind-specific remainder — see decode_rewards.go's
    -- per-kind doc comment for the exact field set landed here.
    attributes         jsonb        NOT NULL DEFAULT '{}'::jsonb,

    ingested_at        timestamptz  NOT NULL DEFAULT now(),

    -- PK includes ledger_close_time (TimescaleDB partition-column
    -- requirement, TS103).
    PRIMARY KEY (ledger_close_time, contract_id, ledger, tx_hash,
                 op_index, event_kind, event_index)
);

COMMENT ON TABLE aquarius_rewards_events IS
    'Aquarius per-pool rewards-gauge / liquidity-mining event surface '
    '(11 kinds: pool_state, claim_reward, set_rewards_config, '
    'position_update, deposit, claim_fees, rewards_gauge_claim, claim, '
    'rewards_gauge_schedule_reward, set_rewards_state, rewards_gauge_add). '
    'Hypertable on ledger_close_time. Aquarius reward tokens have no '
    'published price at this layer; these rows never contribute to VWAP. '
    'See internal/sources/aquarius/README.md (ROADMAP #89).';
COMMENT ON COLUMN aquarius_rewards_events.attributes IS
    'Event-kind-specific remainder. i128 fields are decimal strings '
    '(NUMERIC inside jsonb is lossy per ADR-0003).';

SELECT create_hypertable(
    'aquarius_rewards_events',
    'ledger_close_time',
    chunk_time_interval => INTERVAL '3 days',
    if_not_exists       => TRUE
);

-- Per-pool walk ("this pool's rewards-gauge activity, newest first").
CREATE INDEX aquarius_rewards_events_contract_ts_idx
    ON aquarius_rewards_events (contract_id, ledger_close_time DESC);

-- Per-kind feed ("every claim_reward across Aquarius").
CREATE INDEX aquarius_rewards_events_kind_ts_idx
    ON aquarius_rewards_events (event_kind, ledger_close_time DESC);

-- Per-user walk ("this user's Aquarius rewards history") — partial,
-- most event kinds carry no user_address so a full index would waste
-- space on rows that never match this predicate.
CREATE INDEX aquarius_rewards_events_user_ts_idx
    ON aquarius_rewards_events (user_address, ledger_close_time DESC)
    WHERE user_address IS NOT NULL;

ALTER TABLE aquarius_rewards_events SET (
    timescaledb.compress,
    timescaledb.compress_segmentby = 'contract_id, event_kind',
    timescaledb.compress_orderby   = 'ledger_close_time DESC, ledger DESC'
);

SELECT add_compression_policy(
    'aquarius_rewards_events',
    INTERVAL '7 days',
    if_not_exists => TRUE
);

COMMIT;
