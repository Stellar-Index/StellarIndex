-- 0239 down — drop the lending revenue USD rollup and converge the API
-- role's grants back on 0213's table list (0214's search_path kept).
-- The rollup is derived data: the worker rebuilds it from the lake.

BEGIN;

DROP TABLE IF EXISTS lending_revenue_usd_daily;
DROP TABLE IF EXISTS lending_revenue_usd_coverage;

CREATE OR REPLACE FUNCTION apply_api_role_grants() RETURNS void
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = pg_catalog, public, pg_temp
AS $$
DECLARE
    rw_tables CONSTANT regclass[] := ARRAY[
        'accounts', 'api_keys', 'api_usage_events', 'customer_webhooks',
        'fx_fixings', 'fx_quotes', 'invites', 'login_code_lockouts',
        'magic_link_tokens', 'price_alerts', 'sessions', 'source_entry_counts',
        'status_notices', 'usage_daily', 'users', 'webauthn_credentials',
        'webhook_deliveries'
    ]::regclass[];
    tbl regclass;
    seq regclass;
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'stellarindex_api') THEN
        RETURN;
    END IF;

    REVOKE ALL ON ALL TABLES IN SCHEMA public FROM stellarindex_api;
    REVOKE ALL ON ALL SEQUENCES IN SCHEMA public FROM stellarindex_api;

    GRANT USAGE ON SCHEMA public TO stellarindex_api;
    GRANT SELECT ON ALL TABLES IN SCHEMA public TO stellarindex_api;
    ALTER DEFAULT PRIVILEGES IN SCHEMA public GRANT SELECT ON TABLES TO stellarindex_api;

    FOREACH tbl IN ARRAY rw_tables LOOP
        EXECUTE format('GRANT INSERT, UPDATE, DELETE ON %s TO stellarindex_api', tbl);
    END LOOP;
    GRANT INSERT, UPDATE ON audit_log TO stellarindex_api;
    GRANT INSERT ON erased_account_slugs TO stellarindex_api;

    -- Serial (deptype a) and identity (deptype i) sequences of the tables
    -- the role inserts into.
    FOR seq IN
        SELECT DISTINCT d.objid::regclass
          FROM pg_depend d
          JOIN pg_class s ON s.oid = d.objid AND s.relkind = 'S'
         WHERE d.classid = 'pg_class'::regclass
           AND d.refclassid = 'pg_class'::regclass
           AND d.deptype IN ('a', 'i')
           AND d.refobjid = ANY (rw_tables || ARRAY['audit_log', 'erased_account_slugs']::regclass[])
    LOOP
        EXECUTE format('GRANT USAGE ON SEQUENCE %s TO stellarindex_api', seq);
    END LOOP;
END;
$$;

COMMENT ON FUNCTION apply_api_role_grants() IS
    'Converges role stellarindex_api on SELECT everywhere in public plus '
    'the writes the API process makes, table by table. No-op '
    'while the role does not exist. Migration 0213.';

SELECT apply_api_role_grants();

COMMIT;
