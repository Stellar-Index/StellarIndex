-- 0145 up — admit `treasury_updated` into credit_events.event_type.
--
-- 2026-08-18 ADR-0033 recognition audit (ch-recognition) found one
-- sorocredit event shape classify() never matched: the main contract's
-- `TreasuryUpdated` config event (topic-0 Symbol) — 1 real lake event at
-- ledger 63,847,367, previously dropped end-to-end, tripping
-- recognition_ok=FALSE for the source. Body is Vec[Address old, Address
-- new] (the protocol treasury pointer rotation); it is captured verbatim
-- into credit_events.attributes["body"] exactly like BeaconUpdated /
-- CollateralHashUpdated — no promoted column, no invented semantics.
--
-- Same pattern as 0095 (blend_backstop_events) / 0092/0094 (cctp_events):
-- DROP + re-ADD the CHECK with the full set. Additive + old-binary-safe
-- (migrations rule 9): the previous binary never writes this event_type
-- value, so widening what's ALLOWED doesn't change its behaviour; only the
-- new binary (which now recognises + decodes TreasuryUpdated) emits rows
-- using the new value.
BEGIN;

ALTER TABLE credit_events DROP CONSTRAINT credit_events_event_type_check;  -- migration-compat:ok widening the enum only; every value the old binary writes stays valid, replaced in the same transaction by a strict superset below
ALTER TABLE credit_events ADD CONSTRAINT credit_events_event_type_check  -- migration-compat:ok superset CHECK: adds 'treasury_updated', removes nothing; the previous released binary never writes it (0124 freeze_reason_other parity)
    CHECK (event_type IN (
        'withdrawal', 'beacon_updated',
        'supported_asset_added', 'collateral_hash_updated',
        'treasury_updated'));

COMMIT;
