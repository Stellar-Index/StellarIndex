-- 0134 down — revert classic_assets.slug to the pre-backfill state.
--
-- Before 0134 every row's slug was NULL (the writer bound a literal
-- NULL from 0023 onward; r1 measured 194,057 rows / 0 slugs on
-- 2026-08-04). The up only wrote rows whose slug was NULL, so the down
-- NULLs only the two forms 0134 computes (tier 1 `code-issuer8`
-- lower-cased, tier 2 `CODE-ISSUER`); a slug set any other way is a
-- public URL key 0134 did not write and is kept (GH #1172). Readers
-- COALESCE(slug, code) and the explorer's byAssetId route keeps every
-- asset reachable while slugs are absent.
--
-- NOTE: rolling back the BINARY as well restores the NULL-binding
-- writer; rolling back only this migration leaves new rows arriving
-- with tier-1 slugs again, which is harmless (they are unique).

BEGIN;

UPDATE classic_assets
   SET slug = NULL
 WHERE slug = lower(code) || '-' || lower(left(issuer_g_strkey, 8))
    OR slug = code || '-' || issuer_g_strkey;

COMMIT;
