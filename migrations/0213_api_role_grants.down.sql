-- 0213 down — strip stellarindex_api's grants and drop the function. The
-- role itself is ansible's and stays; an API still pointed at it can then
-- read nothing, so point the API unit back at the owner DSN first.

BEGIN;

DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'stellarindex_api') THEN
        ALTER DEFAULT PRIVILEGES IN SCHEMA public REVOKE ALL ON TABLES FROM stellarindex_api;
        REVOKE ALL ON ALL TABLES IN SCHEMA public FROM stellarindex_api;
        REVOKE ALL ON ALL SEQUENCES IN SCHEMA public FROM stellarindex_api;
        REVOKE USAGE ON SCHEMA public FROM stellarindex_api;
    END IF;
END;
$$;

DROP FUNCTION IF EXISTS apply_api_role_grants();

COMMIT;
