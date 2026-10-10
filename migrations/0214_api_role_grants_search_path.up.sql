-- 0214 up — pin apply_api_role_grants()'s search_path.
--
-- 0213 declared the SECURITY DEFINER function with SET search_path FROM
-- CURRENT, which freezes whatever path the migrating session had. If
-- that path named a schema a less-trusted role can create objects in,
-- the function would resolve pg_roles, format() or a table name there
-- and run it with the owner's rights. Pin the path explicitly: catalog
-- first so built-ins cannot be shadowed, public for the granted tables,
-- pg_temp last so a caller's temp objects are never picked.
--
-- The body needs nothing else: pg_roles, pg_depend, pg_class and format()
-- are pg_catalog; every table name it casts to regclass or grants on
-- lives in public.
--
-- A later CREATE OR REPLACE of this function resets its settings, so it
-- must re-declare this SET search_path or it silently undoes the pin.
--
-- Catalog only; old-binary-safe (rule 9).

BEGIN;

ALTER FUNCTION apply_api_role_grants() SET search_path = pg_catalog, public, pg_temp;

COMMIT;
