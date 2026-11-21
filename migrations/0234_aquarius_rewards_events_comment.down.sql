-- 0234 down — restore 0099's table comment verbatim. COMMENT only.

BEGIN;

COMMENT ON TABLE aquarius_rewards_events IS
    'Aquarius per-pool rewards-gauge / liquidity-mining event surface '
    '(11 kinds: pool_state, claim_reward, set_rewards_config, '
    'position_update, deposit, claim_fees, rewards_gauge_claim, claim, '
    'rewards_gauge_schedule_reward, set_rewards_state, rewards_gauge_add). '
    'Hypertable on ledger_close_time. Aquarius reward tokens have no '
    'published price at this layer; these rows never contribute to VWAP. '
    'See internal/sources/aquarius/README.md (ROADMAP #89).';

COMMIT;
