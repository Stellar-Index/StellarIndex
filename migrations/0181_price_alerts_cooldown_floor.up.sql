-- 0181 up — raise every price alert's cooldown to the 300 s floor.
--
-- WHAT IS WRONG TODAY
--
-- cooldown_seconds accepted 0 (0080: CHECK >= 0, documented as "re-fire
-- every tick"). The evaluator is level-triggered, so such an alert
-- enqueues a delivery to every subscribed webhook on each 30 s tick the
-- condition holds: 25 alerts x 10 webhooks on one free account is 250
-- rows per tick, above the delivery worker's drain (GH #810).
--
-- WHAT THIS MIGRATION CHANGES
--
-- The API now refuses a cooldown below platform.MinAlertCooldownSeconds
-- (300). This raises the rows already stored below it to 300, so the
-- evaluator, the atomic fire claim (last_fired_at + cooldown <= now) and
-- the dashboard all read the same value. updated_at is stamped: the row
-- changed. Values at or above 300 are untouched.
--
-- RULE 9. Data only; no constraint is tightened, so a previous binary
-- keeps reading and writing the same shape (its own inserts below 300
-- are the pre-fix behaviour, not a new failure).

BEGIN;

UPDATE price_alerts
   SET cooldown_seconds = 300,
       updated_at = now()
 WHERE cooldown_seconds < 300;

COMMIT;
