-- 0240 up — the lending emissions USD daily rollup and its coverage.
--
-- One row per (pool, UTC day, stream, line) carrying emission tokens: what
-- GET /v1/lending/pools/{pool}/emissions/history?interval=1d serves for
-- that day, from the same read and valuer. Written only by the
-- stellarindex-api lending emissions rollup worker, under an advisory lock.
-- The streams are layers of the same tokens (scheduled, allocated, claimed)
-- and are never summed: there is no cross-stream column.
--
-- amount_usd is exact, unrounded NUMERIC; the served figure is it rounded
-- once to cents. It is NULL exactly when usd_reason says why, never 0.
-- Inside coverage a missing row means the stream had nothing that day.
--
-- Plain tables: about 30 pools x 1,000 days x a few lines is ~10^5 rows,
-- which needs no chunking, compression or retention.
--
-- Additive: two new empty tables plus the API role's grants converged on
-- them. Old binaries never read them.

BEGIN;

CREATE TABLE lending_emissions_usd_daily (
    pool                    text         NOT NULL,
    day                     date         NOT NULL,
    stream                  text         NOT NULL,
    line                    text         NOT NULL,
    protocol                text         NOT NULL,
    emission_token          text,
    emission_token_decimals smallint,
    asset                   text,
    side                    text,
    events                  bigint,
    amount                  numeric      NOT NULL,
    amount_usd              numeric,
    hours_valued            integer      NOT NULL,
    hours_unvalued          integer      NOT NULL,
    hours_carried           integer      NOT NULL,
    price_basis             text,
    price_basis_hours       jsonb        NOT NULL DEFAULT '{}'::jsonb,
    lower_bound             boolean      NOT NULL,
    stream_lower_bound      boolean      NOT NULL,
    excluded                jsonb        NOT NULL DEFAULT '[]'::jsonb,
    approximate             boolean      NOT NULL,
    approximate_reason      text,
    usd_reason              text,
    valuation_rev           smallint     NOT NULL,
    valued_at               timestamptz  NOT NULL DEFAULT now(),
    PRIMARY KEY (pool, day, stream, line),
    CONSTRAINT lending_emissions_usd_daily_stream
        CHECK (stream IN ('reserve_schedule', 'pool_claims', 'pool_claims_reserve',
                          'backstop_alloc_backstop', 'backstop_alloc_pool', 'backstop_claims')),
    CONSTRAINT lending_emissions_usd_daily_line
        CHECK ((stream IN ('pool_claims', 'backstop_alloc_backstop', 'backstop_alloc_pool')) = (line = '')),
    CONSTRAINT lending_emissions_usd_daily_hours_nonneg
        CHECK (hours_valued >= 0 AND hours_unvalued >= 0
               AND hours_carried >= 0 AND hours_carried <= hours_valued),
    CONSTRAINT lending_emissions_usd_daily_usd_or_reason
        CHECK ((amount_usd IS NULL) = (usd_reason IS NOT NULL)),
    CONSTRAINT lending_emissions_usd_daily_reason_nonblank
        CHECK (usd_reason IS NULL OR btrim(usd_reason) <> ''),
    CONSTRAINT lending_emissions_usd_daily_approximate_reason
        CHECK (approximate = (approximate_reason IS NOT NULL)),
    CONSTRAINT lending_emissions_usd_daily_basis_object
        CHECK (jsonb_typeof(price_basis_hours) = 'object'),
    CONSTRAINT lending_emissions_usd_daily_excluded_array
        CHECK (jsonb_typeof(excluded) = 'array')
);

CREATE INDEX lending_emissions_usd_daily_rev_idx
    ON lending_emissions_usd_daily (valuation_rev);

COMMENT ON TABLE lending_emissions_usd_daily IS
    'Per (pool, UTC day, stream, line) lending emission tokens and their '
    'exact USD, as /emissions/history serves the day at 1d. Written only by '
    'the stellarindex-api lending emissions rollup worker. Streams are '
    'layers of the same tokens and are never summed. Migration 0240.';

CREATE TABLE lending_emissions_usd_coverage (
    pool          text         PRIMARY KEY,
    protocol      text         NOT NULL,
    floor_day     date         NOT NULL,
    through_day   date         NOT NULL,
    unavailable   jsonb        NOT NULL DEFAULT '{}'::jsonb,
    valuation_rev smallint     NOT NULL,
    updated_at    timestamptz  NOT NULL DEFAULT now(),
    CONSTRAINT lending_emissions_usd_coverage_order
        CHECK (through_day >= floor_day - 1),
    CONSTRAINT lending_emissions_usd_coverage_unavailable_object
        CHECK (jsonb_typeof(unavailable) = 'object')
);

COMMENT ON TABLE lending_emissions_usd_coverage IS
    'Per pool, the UTC days the emissions rollup has processed: [floor_day, '
    'through_day]; inside it a missing daily row means the stream had no '
    'tokens that day. unavailable maps each stream the pool does not serve '
    'to its reason. Migration 0240.';

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

    REVOKE ALL ON ALL TABLES IN SCHEMA public FROM stellarindex_api; -- migration-compat:ok 0213's converge: re-granted below in this transaction, a superset of what the released binary has
    REVOKE ALL ON ALL SEQUENCES IN SCHEMA public FROM stellarindex_api; -- migration-compat:ok 0213's converge: re-granted below in this transaction, a superset of what the released binary has

    GRANT USAGE ON SCHEMA public TO stellarindex_api;
    GRANT SELECT ON ALL TABLES IN SCHEMA public TO stellarindex_api;
    ALTER DEFAULT PRIVILEGES IN SCHEMA public GRANT SELECT ON TABLES TO stellarindex_api;

    FOREACH tbl IN ARRAY rw_tables LOOP
        EXECUTE format('GRANT INSERT, UPDATE, DELETE ON %s TO stellarindex_api', tbl);
    END LOOP;
    GRANT INSERT, UPDATE ON audit_log TO stellarindex_api;
    GRANT INSERT ON erased_account_slugs TO stellarindex_api;
    -- The revenue rollup upserts and never deletes.
    GRANT INSERT, UPDATE ON lending_revenue_usd_daily, lending_revenue_usd_coverage TO stellarindex_api;
    -- The emissions rollup replaces a day's rows, so a line that left the day is deleted.
    GRANT INSERT, UPDATE, DELETE ON lending_emissions_usd_daily TO stellarindex_api;
    GRANT INSERT, UPDATE ON lending_emissions_usd_coverage TO stellarindex_api;

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
    'while the role does not exist. Migrations 0213, 0239 and 0240.';

SELECT apply_api_role_grants();

COMMIT;
