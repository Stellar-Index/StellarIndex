-- 0095 up — admit `rw_zone` + `rw_zone_remove` into
-- blend_backstop_events.event_kind.
--
-- 2026-07-09 read-only lake audit (real event bytes vs
-- .discovery-repos/blend-contracts backstop/src/events.rs) found six
-- blend_backstop decode bugs (see internal/sources/blend_backstop's
-- decode.go package doc + CHANGELOG.md). Two of them are new event
-- kinds Classify() never matched at all:
--
--   rw_zone        — the V1 backstop's reward-zone-update topic (V2
--                    renamed this to rw_zone_add); 5 real lake events,
--                    ledgers 51.50M-55.18M, previously dropped
--                    end-to-end.
--   rw_zone_remove — the V2-only pool-removal counterpart to
--                    rw_zone_add; zero lake occurrences ever, added
--                    per the EVERY-event principle (CLAUDE.md) so a
--                    future occurrence doesn't silently fall through.
--
-- Same pattern as 0092/0094 (cctp_events): DROP + re-ADD the CHECK
-- with the full set. Additive + old-binary-safe per rule 9 — the
-- previous binary never writes these event_kind values, so widening
-- what's ALLOWED doesn't change its behavior; only the new binary
-- emits rows using the new values.
BEGIN;

ALTER TABLE blend_backstop_events DROP CONSTRAINT blend_backstop_events_event_kind_check;
ALTER TABLE blend_backstop_events ADD CONSTRAINT blend_backstop_events_event_kind_check CHECK (event_kind IN (
    'deposit', 'claim', 'donate',
    'queue_withdrawal', 'withdraw', 'distribute',
    'gulp_emissions', 'dequeue_withdrawal', 'draw',
    'rw_zone_add', 'rw_zone', 'rw_zone_remove'));

COMMIT;
