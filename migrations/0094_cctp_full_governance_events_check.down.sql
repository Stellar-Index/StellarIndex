-- 0094 down — restore the ten-type CHECK (pre-#89c full census).
-- Any of the 16 newly-admitted event rows must be deleted first or
-- the constraint re-add fails (deliberate: down-migrating with data
-- present should be loud, not silent — same stance as 0070/0092's
-- down).
BEGIN;

DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM cctp_events WHERE event_type IN ( 'admin_change_started', 'attester_enabled', 'attester_manager_updated', 'denylisted', 'denylister_changed', 'fee_recipient_set', 'max_message_body_size_updated', 'min_fee_controller_set', 'pauser_changed', 'rescuer_changed', 'set_burn_limit_per_message', 'set_token_controller', 'signature_threshold_updated', 'swap_minter_config_set', 'token_decimal_config_added', 'un_denylisted' )) THEN
    RAISE EXCEPTION '0094_cctp_full_governance_events_check.down.sql: cctp_events still holds rows where event_type IN ( ''admin_change_started'', ''attester_enabled'', ''attester_manager_updated'', ''denylisted'', ''denylister_changed'', ''fee_recipient_set'', ''max_message_body_size_updated'', ''min_fee_controller_set'', ''pauser_changed'', ''rescuer_changed'', ''set_burn_limit_per_message'', ''set_token_controller'', ''signature_threshold_updated'', ''swap_minter_config_set'', ''token_decimal_config_added'', ''un_denylisted'' ) — down-migrating with data present is LOUD, not silent (#357). Delete them explicitly first if that is really what you want.';
  END IF;
END $$;
ALTER TABLE cctp_events DROP CONSTRAINT cctp_events_event_type_check;
ALTER TABLE cctp_events ADD CONSTRAINT cctp_events_event_type_check CHECK (event_type IN (
    'deposit_for_burn',
    'mint_and_withdraw',
    'message_sent',
    'message_received',
    'mint_and_forward',
    'ownership_transfer',
    'ownership_transfer_completed',
    'admin_changed',
    'remote_token_messenger_added',
    'token_pair_linked'
));

COMMIT;
