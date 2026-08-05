-- 0135 up — classic-asset slugs become the fully-qualified asset_id.
--
-- Supersedes 0134's abbreviated form ONE DAY after it shipped, on the
-- operator's call (2026-08-05): an 8-character issuer prefix is a
-- ~2^32 vanity-grind for an attacker (minutes-to-hours on consumer
-- GPUs — the dust-attack address-mimicry economy does exactly this),
-- and the ONLY thing the abbreviation bought was a shorter URL. The
-- full form is self-certifying — the URL IS the identity, nothing to
-- grind, nothing to front-run — and it deletes the entire collision
-- apparatus: no prefix-tie tiers, no first-observed-wins race, no
-- unique-violation retry in the writer, because the slug equals the
-- primary key's value.
--
-- The catalogue namespace is unaffected: verified currencies keep
-- their human slugs (usdc, aqua) via /v1/assets/verified; this is the
-- CLASSIC-asset namespace only. Nothing external links the day-old
-- 0134 slugs (the explorer deployed against them for ~4 hours), and
-- the API's slug resolver accepts case-folded forms either way.
--
-- Runtime: one pass over ~194k rows of a plain table — seconds.

BEGIN;

UPDATE classic_assets SET slug = asset_id;

COMMIT;
