/**
 * Shared build-time catalogue source for /assets/[slug] static
 * export.
 *
 * Next.js's data-cache opts out of dedup when `signal` is set on
 * fetch, so without a module-level memo every prerendered route
 * would re-fetch `/v1/assets/{slug}` and the build would trip
 * r1's anonymous-tier rate limit. The [slug] route shares this
 * single `/v1/assets/verified` call (retried on 429) to build a
 * slug → GlobalAssetView map.
 *
 * The catalogue listing already carries `ticker`, `slug`, `name`,
 * and `class` per entry — that's everything the static pages need
 * at build time, so per-slug fetches are redundant.
 */

import { API_BASE_URL } from '@/api/client';
import type { components } from '@/api/types';

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

const isCIStub =
  API_BASE_URL.includes('.invalid') || API_BASE_URL.includes('local-stub');
const FETCH_TIMEOUT_MS = 8_000;

let cataloguePromise: Promise<Map<string, GlobalAssetView>> | null = null;

export function getCatalogue(): Promise<Map<string, GlobalAssetView>> {
  if (cataloguePromise) return cataloguePromise;
  cataloguePromise = fetchCatalogueWithRetry();
  return cataloguePromise;
}

async function fetchCatalogueWithRetry(): Promise<Map<string, GlobalAssetView>> {
  if (isCIStub) return new Map();
  const maxAttempts = 5;
  for (let attempt = 0; attempt < maxAttempts; attempt++) {
    try {
      const res = await fetch(`${API_BASE_URL}/v1/assets/verified`, {
        signal: AbortSignal.timeout(FETCH_TIMEOUT_MS),
      });
      if (res.status === 429) {
        const backoffMs = 1000 * (attempt + 1) + Math.floor(Math.random() * 500);
        // eslint-disable-next-line no-console
        console.warn(
          `[catalogue] /v1/assets/verified 429 (attempt ${attempt + 1}/${maxAttempts}); backing off ${backoffMs}ms`,
        );
        await new Promise((r) => setTimeout(r, backoffMs));
        continue;
      }
      if (!res.ok) {
        // eslint-disable-next-line no-console
        console.warn(`[catalogue] /v1/assets/verified http=${res.status}`);
        return new Map();
      }
      const env = (await res.json()) as { data?: VerifiedCurrencyListItem[] };
      const map = new Map<string, GlobalAssetView>();
      for (const item of env.data ?? []) {
        map.set(item.slug, {
          ticker: item.ticker,
          slug: item.slug,
          name: item.name,
          class: item.class,
        });
      }
      return map;
    } catch (err) {
      // eslint-disable-next-line no-console
      console.warn(
        `[catalogue] fetch threw (attempt ${attempt + 1}): ${err instanceof Error ? err.message : String(err)}`,
      );
    }
  }
  return new Map();
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
