-- 0135 down — restore the 0134 abbreviated slugs.
--
-- Re-runs 0134's two-tier computation (see that migration for the tier
-- semantics). Deterministic: the same inputs produce the same slugs
-- 0134 assigned.
--
-- Corrected 2026-09-02 (#357 F10, migrations/README.md "Amending a
-- shipped migration"): this block used to omit 0134's `taken` EXISTS
-- branch, so it was NOT a faithful re-run of 0134 — it was merely
-- equivalent BECAUSE the `UPDATE … SET slug = NULL` above happens to
-- empty the column first, which makes the EXISTS always false. That is
-- a coincidence of statement order, not the tier rule, and anyone who
-- reordered or reused this block would have silently lost tier 2's
-- "the tier-1 form is already taken by an existing slug" case. The
-- branch is restored verbatim from 0134:52-63; the result on a
-- down-migration is byte-identical to before.

BEGIN;

UPDATE classic_assets SET slug = NULL;

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
