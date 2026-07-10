-- 0098 up — admit withdraw_rewards / distribute_rewards into
-- phoenix_stake_events.action; make user_addr + amount nullable.
--
-- ROADMAP #89 residual (2026-07-10): a read-only lake topic census
-- against the gated Phoenix stake-contract set found two real
-- topic[0] action names classifyAny didn't recognize. Real-lake-bytes
-- verified (ledgers 53588319 / 53587626, stake contracts
-- CBRGNWGAC25… / CAF3UJ45ZQJ…; see internal/sources/phoenix's
-- events.go doc + decode.go):
--
--   withdraw_rewards   (40 events) — 2 field-events: user, reward_token.
--   distribute_rewards (18 events) — 1 field-event: asset. Pool-wide —
--                       no user field on the wire.
--
-- Neither event carries an amount. The paid-out amount surfaces on the
-- reward token's own SEP-41 transfer event, emitted as a SEPARATE
-- event in the same op (verified on both real samples: event_index+1
-- from the stake-contract field-events) — correlating against it is
-- out of scope for this pass (would need a cross-decoder join against
-- sep41_transfers). Storing amount=0 would misrepresent a real value
-- as a verified zero, so the column becomes nullable instead.
--
-- Reuses existing columns per the "reuse existing columns" preference:
-- lp_token carries the reward-token / distributed-asset address
-- (repurposed — not an LP share token for these two actions); user_addr
-- carries the withdraw_rewards claimant, NULL for distribute_rewards.
--
-- Column-nullability + CHECK changes are catalog-only (no physical
-- rewrite), the same reasoning 0092/0094/0095/0096-adjacent migrations
-- relied on for CHECK-only ALTERs on compressed hypertables — no
-- decompress_chunk needed (only PK changes require it, per
-- 0053/0054/0058/0060).
BEGIN;

ALTER TABLE phoenix_stake_events ALTER COLUMN user_addr DROP NOT NULL;
ALTER TABLE phoenix_stake_events ALTER COLUMN amount DROP NOT NULL;

ALTER TABLE phoenix_stake_events DROP CONSTRAINT phoenix_stake_events_action_check;
ALTER TABLE phoenix_stake_events ADD CONSTRAINT phoenix_stake_events_action_check CHECK (action IN (
    'bond', 'unbond', 'withdraw_rewards', 'distribute_rewards'
));

COMMENT ON COLUMN phoenix_stake_events.user_addr IS
    'Bond/unbond/withdraw_rewards claimant. NULL for distribute_rewards '
    '(pool-wide announcement, no per-user attribution on the wire).';
COMMENT ON COLUMN phoenix_stake_events.lp_token IS
    'Bond/unbond: the LP share-token address being staked. '
    'withdraw_rewards/distribute_rewards: REPURPOSED to the reward-token '
    '/ distributed-asset address (migration 0098) — same column, '
    'different per-action meaning.';
COMMENT ON COLUMN phoenix_stake_events.amount IS
    'Bond/unbond share-token amount. NULL for withdraw_rewards / '
    'distribute_rewards — neither carries an amount on the event itself '
    '(migration 0098); see internal/sources/phoenix/events.go.';

COMMIT;
