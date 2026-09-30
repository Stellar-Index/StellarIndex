-- 0192 up — admit the Phoenix stake-lifecycle, factory-config and
-- blend-pool events that have a decoder but no storage slot.
--
-- phoenix_stake_events.action gains:
--   create_distribution_flow  ("create_distribution_flow","asset"), body
--                             Address(asset) → lp_token; pool-wide, no user.
--   migration_started         ("Stake: Migration: ","Start of migration for user: ")
--   migration_queried         ("Stake: Migration: ","Query for user completed: ")
--   migration_completed       ("Stake","Migration for user completed and stored: ")
--                             body Address(user) → user_addr; no token, so
--                             lp_token becomes nullable.
--
-- phoenix_admin_events.admin_action gains:
--   factory_config_updated    ("Factory","Updated Config"), body Void.
--   blend_set_delegate        ("blend_pool","set_delegate"), body Address → admin.
--   blend_set_min_trading_a/b ("blend_pool","set_min_trading_a|b"), body
--                             i128 → the new nullable NUMERIC column value.
--
-- Every change is catalog-only (nullability, CHECK, nullable ADD COLUMN
-- with no default), so no chunk is decompressed or rewritten. Both new
-- CHECK sets are strict supersets of the ones they replace.
--
-- Historical fill: `stellarindex-ops projector-replay -source phoenix`
-- from ledger 51,572,016 (docs/protocols/phoenix.md).
BEGIN;

ALTER TABLE phoenix_stake_events ALTER COLUMN lp_token DROP NOT NULL;

ALTER TABLE phoenix_stake_events DROP CONSTRAINT phoenix_stake_events_action_check;
ALTER TABLE phoenix_stake_events ADD CONSTRAINT phoenix_stake_events_action_check CHECK (action IN ( -- migration-compat:ok superset of the 0098 set; every existing row satisfies it
    'bond', 'unbond', 'withdraw_rewards', 'distribute_rewards',
    'create_distribution_flow', 'migration_started', 'migration_queried', 'migration_completed'
));

COMMENT ON COLUMN phoenix_stake_events.user_addr IS
    'Bond/unbond/withdraw_rewards claimant; the migrated user for '
    'migration_*. NULL for distribute_rewards and create_distribution_flow '
    '(pool-wide, no per-user attribution on the wire).';
COMMENT ON COLUMN phoenix_stake_events.lp_token IS
    'Bond/unbond: the LP share-token address being staked. '
    'withdraw_rewards/distribute_rewards/create_distribution_flow: the '
    'reward-token / distributed-asset address — same column, different '
    'per-action meaning. NULL for migration_* (no token on the wire).';
COMMENT ON COLUMN phoenix_stake_events.amount IS
    'Bond/unbond share-token amount. NULL for every other action — none '
    'carries an amount on the event itself; see '
    'internal/sources/phoenix/events.go.';

ALTER TABLE phoenix_admin_events ADD COLUMN value NUMERIC;

ALTER TABLE phoenix_admin_events DROP CONSTRAINT phoenix_admin_events_admin_action_check;
ALTER TABLE phoenix_admin_events ADD CONSTRAINT phoenix_admin_events_admin_action_check CHECK (admin_action IN ( -- migration-compat:ok superset of the 0132 set; every existing row satisfies it
    'replace_requested', 'replace_set', 'undo', 'accepted',
    'factory_config_updated', 'blend_set_delegate',
    'blend_set_min_trading_a', 'blend_set_min_trading_b'
));

COMMENT ON COLUMN phoenix_admin_events.value IS
    'blend_set_min_trading_a/_b: the new minimum trading amount (i128, '
    'token base units). NULL for every other action.';
COMMENT ON TABLE phoenix_admin_events IS
    'Phoenix admin and configuration events: pool admin rotation '
    '(replace_requested / replace_set / undo / accepted), factory config '
    'updates and blend-pool settings. No published price; never '
    'contributes to VWAP. Hypertable on ledger_close_time. See '
    'internal/sources/phoenix/decode_single.go.';

COMMIT;
