-- 0124 up — admit 'other' into the freeze_events.reason CHECK.
--
-- mapFreezeReason (internal/storage/timescale/freeze_events.go) used to
-- map ANY decision that wasn't a phase2/freeze shape to 'manual' — the
-- value 0018 reserved for OPERATOR-initiated freezes — so a defensive
-- fall-through would have recorded an automated decision as a human
-- action on the /v1/anomalies timeline (audit 2026-07-31). The mapper
-- now falls through to 'other', which this migration adds to the
-- constraint's vocabulary. Existing rows are untouched: 'manual' rows
-- written before this change all came from the real operator hatch or
-- the (never-observed) fall-through; there is no way to distinguish
-- them after the fact, so no backfill is attempted.

ALTER TABLE freeze_events DROP CONSTRAINT freeze_events_reason_check;  -- migration-compat:ok widening the reason enum only; every value the old binary writes remains valid
ALTER TABLE freeze_events ADD CONSTRAINT freeze_events_reason_check  -- migration-compat:ok superset CHECK: adds 'other', removes nothing; old binaries never write it and render it as opaque text
    CHECK (reason IN ('single_source','divergence',
                      'outlier_storm','manual','other'));
