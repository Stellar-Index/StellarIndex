-- 0112 up — event_index PK discriminator for cctp_events + rozo_events
-- (audit-2026-07-16 C2-13a).
--
-- The 0038 / 0039 primary keys were
--   (contract_id, ledger, tx_hash, op_index, event_type, ts)
-- on the assumption "one CCTP/Rozo contract emits at most one event per
-- op". That assumption is FALSE: a single operation can emit two events of
-- the SAME event_type (observed on mainnet — e.g. two `attester_enabled`
-- in one MessageTransmitter tx). Both rows share every PK column, so the
-- ON CONFLICT upsert kept ONE and silently dropped the sibling.
--
-- Fix: add event_index — the position of the event within its operation's
-- contract-event list (internal/events.Event.EventIndex, exactly what the
-- soroban_events PK already keys on for the same reason) — and extend the
-- PK with it. ts stays LAST in the key (TimescaleDB requires the partition
-- column in every unique index).
--
-- Historical rows: backfilled to event_index = 0 (the column default).
-- Rows that ALREADY collapsed pre-fix cannot be re-discriminated from
-- storage — recovering their dropped siblings needs a re-ingest of the
-- affected ledger range. This migration is the forward-fix; re-ingest is
-- the history-fix.
--
-- Compressed chunks: cctp_events / rozo_events enable compression
-- CAPABILITY but run NO automatic compression policy (see 0038 / 0039
-- docblocks — "brand-new, low-volume table, no policy"), so in practice
-- there are no compressed chunks to block the PK swap. If a policy is ever
-- added, decompress the affected chunks before applying an equivalent.

BEGIN;

ALTER TABLE cctp_events
    ADD COLUMN event_index integer NOT NULL DEFAULT 0 CHECK (event_index >= 0);
ALTER TABLE cctp_events DROP CONSTRAINT cctp_events_pkey;
ALTER TABLE cctp_events
    ADD CONSTRAINT cctp_events_pkey
    PRIMARY KEY (contract_id, ledger, tx_hash, op_index, event_type, event_index, ts);

ALTER TABLE rozo_events
    ADD COLUMN event_index integer NOT NULL DEFAULT 0 CHECK (event_index >= 0);
ALTER TABLE rozo_events DROP CONSTRAINT rozo_events_pkey;
ALTER TABLE rozo_events
    ADD CONSTRAINT rozo_events_pkey
    PRIMARY KEY (contract_id, ledger, tx_hash, op_index, event_type, event_index, ts);

COMMENT ON COLUMN cctp_events.event_index IS
    'Position of this event within its operation''s contract-event list '
    '(internal/events.Event.EventIndex). PK discriminator so two same-type '
    'events emitted by one op do not collapse (C2-13a). Historical rows are 0.';
COMMENT ON COLUMN rozo_events.event_index IS
    'Position of this event within its operation''s contract-event list '
    '(internal/events.Event.EventIndex). PK discriminator so two same-type '
    'events emitted by one op do not collapse (C2-13a). Historical rows are 0.';

COMMIT;
