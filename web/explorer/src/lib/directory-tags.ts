// Curated third-party issuer-directory labels (account_directory,
// migration 0136 — synced from the MIT-licensed
// stellar-expert/public-directory). Served additively on the asset
// payload as `issuer_directory_tags` / `issuer_directory_domain` /
// `issuer_directory_name`.
//
// DISPLAY-ONLY, third-party attribution — never a StellarIndex
// verification signal. These helpers only decide whether to SURFACE a
// warning; they must never gate price, verification, or ranking.
//
// SERVER-SAFE home (same rationale as asset-label.ts): the RSC asset
// detail page and the 'use client' assets table both import from here.

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
 * a link rather than a broken one.
 */
export function stellarExpertDirectoryUrl(
  issuer: string | null | undefined,
): string | null {
  if (!issuer || !/^G[A-Z2-7]{55}$/.test(issuer)) return null;
  return `https://stellar.expert/explorer/public/account/${issuer}`;
}
