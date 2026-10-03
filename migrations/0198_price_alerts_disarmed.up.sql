-- 0198 up — make price alerts edge-triggered: fire once per crossing.
--
-- WHAT IS WRONG TODAY
--
-- The public contract (`price.alert`, PriceAlertWebhookPayload) says an
-- alert fires when the price CROSSES its threshold, but the evaluator is
-- level-triggered: while the condition holds it re-fires every time the
-- cooldown elapses (GH #664). An "above 0.15" alert on a price that sits
-- above 0.15 for a day notifies 288 times at the 300 s cooldown floor.
--
-- WHAT THIS MIGRATION CHANGES
--
-- Adds `disarmed`: the fire claim sets it (the claim is refused while it
-- is set), and the evaluator clears it once a FRESH closed bucket shows
-- the condition no longer holds. A stale or missing price leaves it as it
-- is. The dashboard clears it when an alert's pair, condition or
-- threshold changes, or when a disabled alert is re-enabled. The cooldown
-- stays as the guard against a price oscillating across the threshold.
--
-- Every row starts armed, so an alert whose condition holds when the new
-- evaluator first runs fires at most once more, after its cooldown. That
-- is preferred over guessing a disarmed state from last_fired_at, which
-- would silence a real crossing the old evaluator never recorded.
--
-- RULE 9. One column with a constant default: catalog-only, no rewrite.
-- The previous binary neither reads nor writes it; its inserts get the
-- default and its claim ignores it, i.e. its own level-triggered
-- behaviour, unchanged.

BEGIN;

ALTER TABLE price_alerts
    ADD COLUMN IF NOT EXISTS disarmed boolean NOT NULL DEFAULT false;

COMMENT ON COLUMN price_alerts.disarmed IS
    'true from a claimed fire until the evaluator sees a fresh price on which the condition no longer holds; a disarmed alert does not fire.';

COMMIT;
