-- 0098 down — restore the two-action CHECK + NOT NULL columns. Any
-- rows using the new actions (or carrying a NULL user_addr/amount)
-- must be deleted first or the constraint/NOT NULL re-add fails
-- (deliberate: down-migrating with data present should be loud, not
-- silent — same stance as 0092/0094/0095's down).
BEGIN;

DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM phoenix_stake_events WHERE action IN ('withdraw_rewards', 'distribute_rewards')) THEN
    RAISE EXCEPTION '0098_phoenix_stake_reward_events.down.sql: phoenix_stake_events still holds rows where action IN (''withdraw_rewards'', ''distribute_rewards'') — down-migrating with data present is LOUD, not silent (#357). Delete them explicitly first if that is really what you want.';
  END IF;
END $$;
ALTER TABLE phoenix_stake_events DROP CONSTRAINT phoenix_stake_events_action_check;
ALTER TABLE phoenix_stake_events ADD CONSTRAINT phoenix_stake_events_action_check CHECK (action IN ('bond', 'unbond'));

ALTER TABLE phoenix_stake_events ALTER COLUMN user_addr SET NOT NULL;
ALTER TABLE phoenix_stake_events ALTER COLUMN amount SET NOT NULL;

COMMENT ON COLUMN phoenix_stake_events.user_addr IS NULL;
COMMENT ON COLUMN phoenix_stake_events.lp_token IS NULL;
COMMENT ON COLUMN phoenix_stake_events.amount IS NULL;

COMMIT;
