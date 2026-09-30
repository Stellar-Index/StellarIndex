-- 0192 down — restore the 0098 / 0132 action sets, lp_token NOT NULL and
-- drop phoenix_admin_events.value. Rows using the new actions must be
-- deleted first: down-migrating with data present is loud, not silent.
BEGIN;

DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM phoenix_stake_events WHERE action IN ('create_distribution_flow', 'migration_started', 'migration_queried', 'migration_completed')) THEN
    RAISE EXCEPTION '0192_phoenix_stake_lifecycle_and_config_events.down.sql: phoenix_stake_events still holds create_distribution_flow / migration_* rows. Delete them explicitly first if that is really what you want.';
  END IF;
  IF EXISTS (SELECT 1 FROM phoenix_admin_events WHERE admin_action IN ('factory_config_updated', 'blend_set_delegate', 'blend_set_min_trading_a', 'blend_set_min_trading_b')) THEN
    RAISE EXCEPTION '0192_phoenix_stake_lifecycle_and_config_events.down.sql: phoenix_admin_events still holds factory_config_updated / blend_set_* rows. Delete them explicitly first if that is really what you want.';
  END IF;
END $$;

ALTER TABLE phoenix_admin_events DROP CONSTRAINT phoenix_admin_events_admin_action_check;
ALTER TABLE phoenix_admin_events ADD CONSTRAINT phoenix_admin_events_admin_action_check CHECK (admin_action IN (
    'replace_requested', 'replace_set', 'undo', 'accepted'
));
ALTER TABLE phoenix_admin_events DROP COLUMN value;
COMMENT ON TABLE phoenix_admin_events IS
    'Phoenix pool admin-rotation governance events (replace_requested / '
    'replace_set / undo / accepted). No published price; never '
    'contributes to VWAP. 0 occurrences to date — built defensively. '
    'Hypertable on ledger_close_time. See internal/sources/phoenix/decode.go.';

ALTER TABLE phoenix_stake_events DROP CONSTRAINT phoenix_stake_events_action_check;
ALTER TABLE phoenix_stake_events ADD CONSTRAINT phoenix_stake_events_action_check CHECK (action IN (
    'bond', 'unbond', 'withdraw_rewards', 'distribute_rewards'
));
ALTER TABLE phoenix_stake_events ALTER COLUMN lp_token SET NOT NULL;

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
