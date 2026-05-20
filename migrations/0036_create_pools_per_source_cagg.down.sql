-- 0036 down — drop the per-source pools CAGG.
--
-- Removes the policy first (TimescaleDB requires this before
-- dropping a continuous aggregate that has one attached), then the
-- materialized view itself. CASCADE handles any future code that
-- joined against the CAGG (today nothing does — the handler
-- transition lives in a separate commit so this DOWN is safe to
-- run before that commit lands).

BEGIN;

SELECT remove_continuous_aggregate_policy('pools_per_source_1h', if_exists => true);

DROP MATERIALIZED VIEW IF EXISTS pools_per_source_1h CASCADE;

COMMIT;
