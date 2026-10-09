-- 0213 up — least-privilege grants for the API's own Postgres role.
--
-- Every binary connects as the `stellarindex` role, which owns every
-- table (rule 7). The single-writer invariant (projected domains written
-- only by internal/projector, chain data only by the indexer/dispatcher)
-- is therefore convention: one stray or injected statement in the public
-- API process could rewrite trades or a projected table.
--
-- This migration defines apply_api_role_grants(), which shapes the
-- privileges of the `stellarindex_api` login role:
--
--   * SELECT on every table, view and continuous aggregate in public,
--     including ones later migrations create (default privileges).
--   * INSERT / UPDATE / DELETE only on the tables the stellarindex-api
--     process itself writes: the platform tables (accounts, keys,
--     sessions, webhooks, alerts, audit trail), usage_daily, and the
--     forex worker's fx_quotes / fx_fixings / source_entry_counts.
--   * USAGE on sequences, so those INSERTs can draw ids.
--   * No TRUNCATE, no DDL, no write on anything else.
--
-- It starts with REVOKE ALL, so calling it again converges the role on
-- exactly this list. A later migration that adds a table the API writes
-- must CREATE OR REPLACE this function with the table added and call it.
--
-- The role is created by ansible (05-postgres.yml), not here: migrations
-- run as `stellarindex`, which has no CREATEROLE. Where the role does not
-- exist yet the function is a no-op; ansible calls it once the role
-- exists. SECURITY DEFINER so a superuser caller still grants as, and
-- sets default privileges for, the owning role.
--
-- Rule-9 safe: catalog only. No running binary connects as
-- stellarindex_api until the operator points the API unit at it.

BEGIN;

CREATE FUNCTION apply_api_role_grants() RETURNS void
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path FROM CURRENT
AS $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'stellarindex_api') THEN
        RETURN;
    END IF;

    REVOKE ALL ON ALL TABLES IN SCHEMA public FROM stellarindex_api;
    REVOKE ALL ON ALL SEQUENCES IN SCHEMA public FROM stellarindex_api;

    GRANT USAGE ON SCHEMA public TO stellarindex_api;
    GRANT SELECT ON ALL TABLES IN SCHEMA public TO stellarindex_api;
    GRANT USAGE, SELECT ON ALL SEQUENCES IN SCHEMA public TO stellarindex_api;
    ALTER DEFAULT PRIVILEGES IN SCHEMA public GRANT SELECT ON TABLES TO stellarindex_api;
    ALTER DEFAULT PRIVILEGES IN SCHEMA public GRANT USAGE, SELECT ON SEQUENCES TO stellarindex_api;

    GRANT INSERT, UPDATE, DELETE ON
        accounts,
        api_keys,
        api_usage_events,
        audit_log,
        customer_webhooks,
        erased_account_slugs,
        fx_fixings,
        fx_quotes,
        invites,
        login_code_lockouts,
        magic_link_tokens,
        price_alerts,
        sessions,
        source_entry_counts,
        status_notices,
        usage_daily,
        users,
        webauthn_credentials,
        webhook_deliveries
    TO stellarindex_api;
END;
$$;

REVOKE EXECUTE ON FUNCTION apply_api_role_grants() FROM PUBLIC;

COMMENT ON FUNCTION apply_api_role_grants() IS
    'Converges role stellarindex_api on SELECT everywhere in public plus '
    'INSERT/UPDATE/DELETE on the tables the API process writes. No-op '
    'while the role does not exist. Migration 0213.';

SELECT apply_api_role_grants();

COMMIT;
