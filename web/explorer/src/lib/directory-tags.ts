// Curated third-party issuer-directory labels (account_directory,
// migration 0136 — synced from the MIT-licensed
// stellar-expert/public-directory). Served additively on the asset
// payload as `issuer_directory_tags` / `issuer_directory_domain` /
// `issuer_directory_name`.
//
// Third-party attribution — never a StellarIndex verification signal and
// never an input to the verified catalogue.
//
// TWO deliberate exceptions to the original display-only rule, both
// scoped to the SCAM-CLASS tags below:
//
//   - the server withholds price + market cap for a flagged issuer
//     (pricingguard.ScamGate, 2026-08-25); and
//   - a flagged asset is RANKED LAST — server-side in the listing's
//     ORDER BY, and here by demoteFlaggedLast for whatever column the
//     user sorts the rendered page by (#356). The row and its badge
//     stay: we do not hide a flagged asset, we refuse to rank it.
//
// SERVER-SAFE home (same rationale as asset-label.ts): the RSC asset
// detail page and the 'use client' assets table both import from here.

import { stellarExpertUrl } from '@/lib/networks';

/**
 * DIRECTORY_SCAM_FLAG_TAGS — the upstream directory tags that warrant a
 * prominent scam warning on an asset page/row. Matched case-insensitively
 * against the served tags.
 */
export const DIRECTORY_SCAM_FLAG_TAGS = [
  'malicious',
  'unsafe',
  'fraud',
  'scam',
  'hack',
  'phishing',
] as const;

const FLAG_SET = new Set<string>(DIRECTORY_SCAM_FLAG_TAGS);

/**
 * scamFlagTags returns the subset of the issuer's directory tags that are
 * scam-warning flags, preserving the served (original-case) tag strings
 * and their order. Empty when the asset carries no such flag.
 */
export function scamFlagTags(
  tags: readonly string[] | null | undefined,
): string[] {
  if (!tags) return [];
  return tags.filter((t) => FLAG_SET.has(t.toLowerCase()));
}

/**
 * hasDirectoryScamFlag — true when the issuer's directory tags include
 * any scam-warning flag. Drives the detail-page banner + table badge.
 */
export function hasDirectoryScamFlag(
  tags: readonly string[] | null | undefined,
): boolean {
  return scamFlagTags(tags).length > 0;
}

/**
 * stellarExpertDirectoryUrl — the third-party directory source link for
 * an issuer G-address (the stellar.expert public directory / account
 * page). Fixed, trusted host; the issuer is path-encoded. Returns null
 * for a missing/malformed G-strkey so callers render attribution without
 * a link rather than a broken one — and likewise on a network
 * stellar.expert does not host (see stellarExpertUrl).
 */
export function stellarExpertDirectoryUrl(
  issuer: string | null | undefined,
): string | null {
  if (!issuer || !/^G[A-Z2-7]{55}$/.test(issuer)) return null;
  return stellarExpertUrl('account', issuer);
}

/**
 * demoteFlaggedLast — stable partition that moves every directory-flagged
 * row below every unflagged one, preserving the incoming order within each
 * group.
 *
 * The server already ranks flagged assets last (the listing's rank_tier
 * ORDER BY key), but a table whose headers re-sort the fetched page
 * client-side would float a flagged row straight back to the top the
 * moment the user clicks "Volume 24h" — the demotion has to survive
 * "whatever the active sort key" (#356). Applying it AFTER the column sort
 * is exactly equivalent to making flagged-ness the primary sort key, and
 * leaves the shared useTableSort hook untouched for every other table.
 *
 * `tagsOf` reads the row's issuer_directory_tags, so this stays generic
 * over row shapes.
 */
export function demoteFlaggedLast<T>(
  rows: readonly T[],
  tagsOf: (row: T) => readonly string[] | null | undefined,
): T[] {
  const unflagged: T[] = [];
  const flagged: T[] = [];
  for (const row of rows) {
    (hasDirectoryScamFlag(tagsOf(row)) ? flagged : unflagged).push(row);
  }
  return flagged.length === 0 ? unflagged : [...unflagged, ...flagged];
}
