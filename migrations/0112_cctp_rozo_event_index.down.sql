-- 0112 down — revert the event_index PK discriminator (C2-13a).
--
-- NOTE: this can only succeed while no op has actually stored two
-- same-type events (distinct event_index, otherwise-identical PK). Once the
-- forward fix has let such sibling rows land, narrowing the PK back would
-- violate uniqueness — that data used the new capability and cannot be
-- un-split. On a fresh / uncollapsed table (the migration-test shape) the
-- rollback is clean.

BEGIN;

ALTER TABLE cctp_events DROP CONSTRAINT cctp_events_pkey;
ALTER TABLE cctp_events
    ADD CONSTRAINT cctp_events_pkey
    PRIMARY KEY (contract_id, ledger, tx_hash, op_index, event_type, ts);
ALTER TABLE cctp_events DROP COLUMN event_index;

ALTER TABLE rozo_events DROP CONSTRAINT rozo_events_pkey;
ALTER TABLE rozo_events
    ADD CONSTRAINT rozo_events_pkey
    PRIMARY KEY (contract_id, ledger, tx_hash, op_index, event_type, ts);
ALTER TABLE rozo_events DROP COLUMN event_index;

COMMIT;
