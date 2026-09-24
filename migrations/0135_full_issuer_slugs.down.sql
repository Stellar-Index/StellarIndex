-- 0135 down — restore the 0134 abbreviated slugs.
--
-- Re-runs 0134's two-tier computation (see that migration for the tier
-- semantics). Deterministic: the same inputs produce the same slugs
-- 0134 assigned.
--
-- Only slugs equal to asset_id (the form 0135 and its writer produce)
-- are cleared and recomputed. A slug set any other way is a public URL
-- key 0135 did not write and is kept (GH #1172); 0134's `taken` branch
-- (copied verbatim from 0134) then routes a tier-1 form it already
-- occupies to tier 2 instead of violating the UNIQUE constraint.

BEGIN;

UPDATE classic_assets SET slug = NULL WHERE slug = asset_id;

WITH computed AS (
    SELECT ca.asset_id,
           lower(ca.code) || '-' || lower(left(ca.issuer_g_strkey, 8)) AS base_slug,
           row_number() OVER (
               PARTITION BY lower(ca.code) || '-' || lower(left(ca.issuer_g_strkey, 8))
               ORDER BY ca.asset_id
           ) AS rn,
           EXISTS (
               SELECT 1 FROM classic_assets e
                WHERE e.slug = lower(ca.code) || '-' || lower(left(ca.issuer_g_strkey, 8))
           ) AS taken
      FROM classic_assets ca
     WHERE ca.slug IS NULL
)
UPDATE classic_assets ca
   SET slug = CASE
                  WHEN c.rn = 1 AND NOT c.taken THEN c.base_slug
                  ELSE ca.code || '-' || ca.issuer_g_strkey
              END
  FROM computed c
 WHERE ca.asset_id = c.asset_id;

COMMIT;
