-- 0198 down — drop price_alerts.disarmed. Alerts return to firing every
-- cooldown while the condition holds; no other column is touched.

BEGIN;

ALTER TABLE price_alerts
    DROP COLUMN IF EXISTS disarmed;

COMMIT;
