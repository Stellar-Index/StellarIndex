-- 0192 down — drop fx_fixings
--
-- Development lever only: the table is the audit trail of every served
-- conversion, so production never runs this.

BEGIN;

DROP TABLE IF EXISTS fx_fixings;

COMMIT;
