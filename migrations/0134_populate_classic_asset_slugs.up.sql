-- 0134 up — populate classic_assets.slug (2026-08-04 identity incident).
--
-- Migration 0023 created `slug text UNIQUE` and DOCUMENTED the scheme
-- ("auto-generated lowercase code; disambiguated with a short issuer
-- prefix when multiple issuers issue assets with the same code") — but
-- the writer (asset_registry.go) bound a literal NULL for its whole
-- life. r1 measurement 2026-08-04: 194,057 rows, 0 slugs. Every reader
-- COALESCEs slug to CODE, so the bare CODE became the de-facto public
-- slug: 5+ unrelated issuers of "USDT" all published slug "USDT", and
-- the explorer's slug-keyed build cache crowned an arbitrary winner as
-- /assets/USDT — a dead bridge's discount rendered as Tether's price.
--
-- This backfill implements the documented scheme, tightened by the
-- 2026-08-04 identity decision (assets are identified by
-- (code, issuer), NEVER code alone): EVERY classic asset gets a
-- disambiguated slug — the bare code is never a classic asset's slug,
-- not even for a code with a single issuer. A bare-code URL now
-- resolves through the verified-currency catalogue (which owns
-- human-name slugs) or 404s, exactly like the API already behaves.
--
-- Two deterministic tiers (the header said "three" and then enumerated
-- two, matching the two CASE branches below — corrected 2026-09-02,
-- #357 F10), computed in ONE statement because slug is
-- UNIQUE and a multi-statement backfill could trip its own constraint
-- mid-way:
--
--   tier 1: lower(code) || '-' || lower(first 8 chars of issuer)
--           ("usdt-gcqtgzqq") — short, URL-stable, and unique unless
--           two same-code issuers share an 8-char strkey prefix.
--   tier 2: rows losing the tier-1 uniqueness race (row_number > 1
--           within a tier-1 partition, or the tier-1 form already
--           taken by an existing slug) fall back to the fully-
--           qualified asset_id (code || '-' || issuer) — unique by
--           construction, since it is the primary key's value.
--
-- The write path stamps the same tier-1 form on new rows (with the
-- same asset_id fallback on a unique violation) as of the same
-- release — see asset_registry.go. ON CONFLICT the writer keeps the
-- existing slug: a slug is a public URL and must never silently
-- change once issued.
--
-- Runtime: one pass over ~194k rows of a plain (non-hypertable) table
-- with a window function — seconds, no compression interplay.

BEGIN;

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
