-- 0124 down — restore the four-value reason CHECK. Fails (by design) if
-- any 'other' rows have been recorded; fold them into 'manual' first if
-- the rollback is genuinely needed.

ALTER TABLE freeze_events DROP CONSTRAINT freeze_events_reason_check;
ALTER TABLE freeze_events ADD CONSTRAINT freeze_events_reason_check
    CHECK (reason IN ('single_source','divergence',
                      'outlier_storm','manual'));
