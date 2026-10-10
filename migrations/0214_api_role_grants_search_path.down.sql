-- 0214 down — restore 0213's SET search_path FROM CURRENT, which captures
-- the migrating session's path as 0213 did.

BEGIN;

ALTER FUNCTION apply_api_role_grants() SET search_path FROM CURRENT;

COMMIT;
