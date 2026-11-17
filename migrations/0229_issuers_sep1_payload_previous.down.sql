-- 0229 down — drop issuers.sep1_payload_previous. A payload held there for
-- a domain not yet re-fetched is lost, and that fetch then records no
-- history, as before 0229.

BEGIN;

SET LOCAL lock_timeout = '5s';

ALTER TABLE issuers DROP COLUMN IF EXISTS sep1_payload_previous;

COMMIT;
