-- 0234 up — correct aquarius_rewards_events' table comment: the event_kind
-- CHECK admits 12 kinds, and config_rewards is emitted by the Aquarius
-- router, so not every row's contract_id is a pool. 0099's comment listed
-- 11 and called every row per-pool.
--
-- COMMENT only: catalog write under SHARE UPDATE EXCLUSIVE on the root
-- table, no chunk touched, sub-second. Re-run converges.

BEGIN;

COMMENT ON TABLE aquarius_rewards_events IS
    'Aquarius rewards-gauge / liquidity-mining event surface (12 kinds: '
    'pool_state, claim_reward, set_rewards_config, position_update, deposit, '
    'claim_fees, rewards_gauge_claim, claim, rewards_gauge_schedule_reward, '
    'set_rewards_state, rewards_gauge_add, config_rewards). contract_id is the '
    'pool for every kind except config_rewards, which the router emits with '
    'contract_id = router. Hypertable on ledger_close_time. Aquarius reward '
    'tokens have no published price at this layer; these rows never '
    'contribute to VWAP. See internal/sources/aquarius/README.md.';

COMMIT;
