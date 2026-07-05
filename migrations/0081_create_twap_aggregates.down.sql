-- 0081 down — drop the twap_1h + twap_1d continuous aggregates.
-- DROP MATERIALIZED VIEW removes each CAGG, its refresh policy, and
-- its pair/bucket index in one step. Dropped before prices_1m (0002
-- down runs later in the rollback order), so the hierarchical parent
-- goes away while its prices_1m source still exists — no dependency
-- error.

BEGIN;

DROP MATERIALIZED VIEW IF EXISTS twap_1d;
DROP MATERIALIZED VIEW IF EXISTS twap_1h;

COMMIT;
