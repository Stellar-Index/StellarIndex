-- 0135 down — restore the 0134 abbreviated slugs.
--
-- Re-runs 0134's three-tier computation (see that migration for the
-- tier semantics). Deterministic: the same inputs produce the same
-- slugs 0134 assigned.

BEGIN;

UPDATE classic_assets SET slug = NULL;

WITH computed AS (
    SELECT ca.asset_id,
           lower(ca.code) || '-' || lower(left(ca.issuer_g_strkey, 8)) AS base_slug,
           row_number() OVER (
               PARTITION BY lower(ca.code) || '-' || lower(left(ca.issuer_g_strkey, 8))
               ORDER BY ca.asset_id
           ) AS rn
      FROM classic_assets ca
     WHERE ca.slug IS NULL
)
UPDATE classic_assets ca
   SET slug = CASE
                  WHEN c.rn = 1 THEN c.base_slug
                  ELSE ca.code || '-' || ca.issuer_g_strkey
              END
  FROM computed c
 WHERE ca.asset_id = c.asset_id;

COMMIT;
