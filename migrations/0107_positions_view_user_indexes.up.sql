-- 0107 up — user-leading indexes for GET /v1/accounts/{g}/positions
-- (the "DeFi positions" view).
--
-- The new endpoint folds six protocols' event tables down to a
-- per-(user, venue) net position. Four of those tables already carry a
-- user-leading index the fold's `WHERE user_col = $1` can use directly:
--   phoenix_stake_events_user_ts_idx   (user_addr, ledger_close_time DESC)         — migration 0044
--   defindex_flows_actor_ts_idx        (actor, ledger_close_time DESC)             — migration 0050
--   credit_positions_owner_ts_idx      (owner, ledger_close_time DESC)             — migration 0090
--   aquarius_rewards_events_user_ts_idx (user_address, ledger_close_time DESC)
--     WHERE user_address IS NOT NULL                                              — migration 0099
--
-- Two do NOT:
--   blend_positions — the only user-touching index is
--     blend_positions_pool_user_asset_idx (pool, user_address, asset, …),
--     POOL-leading — useless for "every position event for this user
--     across every pool" (migration 0045/0053). A plain
--     `WHERE user_address = $1` would seq-scan the hypertable — the
--     exact "no unbounded trade-scan queries" failure mode CLAUDE.md /
--     feedback_no_unbounded_trade_scan.md already learned the hard way
--     once (sep41_transfers, migration 0106, same shape).
--   blend_backstop_events — every existing index leads with contract_id,
--     event_kind, pool (partial), or ledger (migration 0063); NONE leads
--     with user_address.
--
-- Both new indexes are partial on their (already effectively-required)
-- user column: blend_positions.user_address is NOT NULL so the WHERE
-- clause is a formality that keeps the index shape symmetric with its
-- blend_backstop_events sibling (whose user_address genuinely IS
-- nullable — 5 of 12 event kinds carry no user, migration 0063).
--
-- No CONCURRENTLY (matches 0106's own convention): r1 gets a by-hand
-- CONCURRENTLY index ahead of this migration; this file's plain form is
-- what a fresh/dev deployment's migration run actually executes and is
-- a safe no-op on r1 once the by-hand index exists.

BEGIN;

CREATE INDEX IF NOT EXISTS blend_positions_user_ts_idx
    ON blend_positions (user_address, ledger_close_time DESC)
    WHERE user_address IS NOT NULL;

CREATE INDEX IF NOT EXISTS blend_backstop_events_user_ts_idx
    ON blend_backstop_events (user_address, ledger_close_time DESC)
    WHERE user_address IS NOT NULL;

COMMIT;
