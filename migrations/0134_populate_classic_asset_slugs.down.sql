-- 0134 down — revert classic_assets.slug to the pre-backfill state.
--
-- Before 0134 every row's slug was NULL (the writer bound a literal
-- NULL from 0023 onward; r1 measured 194,057 rows / 0 slugs on
-- 2026-08-04), so NULLing the column is a faithful revert. Readers
-- COALESCE(slug, code) and the explorer's byAssetId route keeps every
-- asset reachable while slugs are absent.
--
-- NOTE: rolling back the BINARY as well restores the NULL-binding
-- writer; rolling back only this migration leaves new rows arriving
-- with tier-1 slugs again, which is harmless (they are unique).

BEGIN;

UPDATE classic_assets SET slug = NULL;

COMMIT;
