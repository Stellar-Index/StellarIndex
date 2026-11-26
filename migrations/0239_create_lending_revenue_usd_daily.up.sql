-- 0239 up — the lending revenue USD daily rollup and its coverage.
--
-- One row per (pool, reserve asset, UTC day) with interest rows: the day's
-- seven revenue legs in USD, each hour valued at the price a walk of that
-- UTC day gives (the /revenue valuer, day by day). Written only by the
-- stellarindex-api lending revenue rollup worker, under an advisory lock.
--
-- Amounts are exact, unrounded NUMERIC with no scale: a day is a sum of
-- base units times a decimal price over 10^decimals, and nothing of it may
-- be rounded before a reader sums days. The legs are NULL together when no
-- hour carrying an amount was priced (a day with nothing to value is 0);
-- walk_reason then says why when the price
-- data itself answers (unpriced, decimals_unknown, price_scale_mismatch).
-- A day a read-time verdict refused (scam issuer, thin latest market,
-- operator withholding) is stored with its legs NULL, every hour carrying
-- an amount unpriced and walk_reason 'withheld' (never 0), and the daily
-- sweep re-checks it. A day whose walk failed or was cut short has no row
-- and is retried; coverage stops before the first such day the self-fill
-- meets, so inside coverage a missing row means the day had no interest.
-- A re-derive that only deletes an hour's superseded rows writes no newer
-- derived_at; the daily sweep is what catches it.
--
-- interest_derived_at is the newest derived_at of the day's interest hour
-- rows, read before them, so a later re-derive is always seen as newer.
--
-- Plain tables: about 110 reserves x 1,000 days is ~10^5 rows, which
-- needs no chunking, compression or retention.
--
-- Additive: two new empty tables plus the API role's grants converged
-- on them. Old binaries never read them.

BEGIN;

CREATE TABLE lending_revenue_usd_daily (
    pool                      text         NOT NULL,
    asset                     text         NOT NULL,
    day                       date         NOT NULL,
    interest_derive_rev       smallint     NOT NULL,
    valuation_rev             smallint     NOT NULL,
    interest_derived_at       timestamptz  NOT NULL,
    interest_accrued_usd      numeric,
    supplier_share_usd        numeric,
    supplier_credited_usd     numeric,
    backstop_take_usd         numeric,
    protocol_fee_usd          numeric,
    backstop_drawn_usd        numeric,
    default_loss_usd          numeric,
    priced_hours              integer      NOT NULL, -- lint-money:ok an hour count
    unpriced_hours            integer      NOT NULL, -- lint-money:ok an hour count
    price_carried_hours       integer      NOT NULL, -- lint-money:ok an hour count
    price_basis_hours         jsonb        NOT NULL DEFAULT '{}'::jsonb,
    stale_rows                bigint       NOT NULL DEFAULT 0,
    accrual_excluded_versions bigint       NOT NULL DEFAULT 0,
    unexplained_loss_versions bigint       NOT NULL DEFAULT 0,
    walk_reason               text,
    valued_at                 timestamptz  NOT NULL DEFAULT now(),
    PRIMARY KEY (pool, asset, day),
    CONSTRAINT lending_revenue_usd_daily_hours_nonneg
        CHECK (priced_hours >= 0 AND unpriced_hours >= 0
               AND price_carried_hours >= 0 AND price_carried_hours <= priced_hours),
    CONSTRAINT lending_revenue_usd_daily_legs_together
        CHECK ((interest_accrued_usd IS NULL) = (priced_hours = 0 AND unpriced_hours > 0)
           AND (interest_accrued_usd IS NULL) = (supplier_share_usd IS NULL)
           AND (interest_accrued_usd IS NULL) = (supplier_credited_usd IS NULL)
           AND (interest_accrued_usd IS NULL) = (backstop_take_usd IS NULL)
           AND (interest_accrued_usd IS NULL) = (protocol_fee_usd IS NULL)
           AND (interest_accrued_usd IS NULL) = (backstop_drawn_usd IS NULL)
           AND (interest_accrued_usd IS NULL) = (default_loss_usd IS NULL)),
    CONSTRAINT lending_revenue_usd_daily_reason_nonblank
        CHECK (walk_reason IS NULL OR btrim(walk_reason) <> ''),
    CONSTRAINT lending_revenue_usd_daily_basis_object
        CHECK (jsonb_typeof(price_basis_hours) = 'object')
);

-- The staleness pass picks rows of an older revision first.
CREATE INDEX lending_revenue_usd_daily_revs_idx
    ON lending_revenue_usd_daily (valuation_rev, interest_derive_rev);

COMMENT ON TABLE lending_revenue_usd_daily IS
    'Per (pool, asset, UTC day) lending revenue in exact USD, each hour at '
    'the price a walk of that day gives. Written only by the stellarindex-api '
    'lending revenue rollup worker. Legs NULL together when no hour was '
    'priced. price_basis_hours counts priced hours per basis, fiat as "fiat". '
    'Migration 0239.';

CREATE TABLE lending_revenue_usd_coverage (
    pool                text         PRIMARY KEY,
    floor_day           date         NOT NULL,
    through_day         date         NOT NULL,
    redo_from_day       date,
    interest_derive_rev smallint     NOT NULL,
    valuation_rev       smallint     NOT NULL,
    updated_at          timestamptz  NOT NULL DEFAULT now(),
    CONSTRAINT lending_revenue_usd_coverage_order
        CHECK (through_day >= floor_day - 1),
    CONSTRAINT lending_revenue_usd_coverage_redo_order
        CHECK (redo_from_day IS NULL OR redo_from_day >= floor_day)
);

COMMENT ON TABLE lending_revenue_usd_coverage IS
    'Per pool, the UTC days the rollup has processed: [floor_day, '
    'through_day], from the first wholly derived day through the day before '
    'the first one the rollup could not value; inside it a missing daily row '
    'means no interest that day. Days from redo_from_day are suspect until a '
    'pending re-derive rewrites them. Migration 0239.';

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
    -- The rollup worker upserts and never deletes.
    GRANT INSERT, UPDATE ON lending_revenue_usd_daily, lending_revenue_usd_coverage TO stellarindex_api;

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
    'while the role does not exist. Migrations 0213 and 0239.';

SELECT apply_api_role_grants();

COMMIT;
