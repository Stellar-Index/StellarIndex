/**
 * Shared build-time catalogue source for /assets/[slug] static
 * export.
 *
 * The [slug] route shares this single `/v1/assets/verified` call
 * (memoised per build via buildFetch) to build a slug →
 * GlobalAssetView map. Per-slug `/v1/assets/{slug}` fetches at build
 * time would 429 in parallel against r1's anon-tier rate limit.
 *
 * The catalogue listing already carries `ticker`, `slug`, `name`,
 * and `class` per entry — that's everything the static pages need
 * at build time, so per-slug fetches are redundant.
 *
 * Fail-hard: a real build with an unreachable/failing catalogue
 * endpoint throws (via buildFetch) rather than exporting the verified
 * asset pages half-empty. Only CI-stub builds get the empty map.
 */

import type { components } from '@/api/types';
import { buildFetchData, failBuild } from '@/lib/buildFetch';

// Wire shapes from the generated OpenAPI contract (src/api/types.ts).
type VerifiedCurrencyListItem = components['schemas']['VerifiedCurrencyListItem'];

export type GlobalAssetView = components['schemas']['GlobalAssetView'] & {
  // SPEC-GAP: /v1/assets/{slug}'s GlobalAssetView carries `class`
  // (internal/api/v1/assets_global.go GlobalAssetView.Class, required)
  // but the spec's GlobalAssetView schema omits it. Optional here
  // because this catalogue map is also built from the /v1/assets/verified
  // listing where the field is copied over client-side.
  class?: VerifiedCurrencyListItem['class'];
};

let cataloguePromise: Promise<Map<string, GlobalAssetView>> | null = null;

export function getCatalogue(): Promise<Map<string, GlobalAssetView>> {
  if (cataloguePromise) return cataloguePromise;
  cataloguePromise = fetchCatalogue();
  return cataloguePromise;
}

async function fetchCatalogue(): Promise<Map<string, GlobalAssetView>> {
  const rows = await buildFetchData<VerifiedCurrencyListItem[]>(
    '/v1/assets/verified',
  );
  const map = new Map<string, GlobalAssetView>();
  if (!rows || rows.length === 0) {
    // null/empty = CI stub (render fallbacks) or a 4xx/empty payload on
    // a listing endpoint that always has rows — fail the build.
    failBuild('/v1/assets/verified returned no catalogue rows at build time');
    return map;
  }
  for (const item of rows) {
    map.set(item.slug, {
      ticker: item.ticker,
      slug: item.slug,
      name: item.name,
      class: item.class,
    });
  }
  return map;
}

/**
 * Case-insensitive catalogue lookup. generateStaticParams emits
 * multiple case variants per slug; all should resolve to the
 * canonical lowercase entry.
 */
export async function lookupGlobalAsset(
  slug: string,
): Promise<GlobalAssetView | null> {
  const map = await getCatalogue();
  return map.get(slug) ?? map.get(slug.toLowerCase()) ?? null;
}
