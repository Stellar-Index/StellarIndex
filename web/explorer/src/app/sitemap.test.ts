import { afterEach, describe, expect, it, vi } from 'vitest';

/**
 * No API-derived sitemap section may wrap exactly one
 * 5s-timeout `fetch` in try/catch and fall back to `[]` on ANY failure —
 * no retry, so a single transient blip would permanently drop a whole URL
 * family from the sitemap with no error and no reconciliation against a
 * prior good result. sitemap.ts goes through buildFetch
 * (src/lib/buildFetch.ts), the same bounded-retry/fail-hard layer every
 * sibling generateStaticParams uses for these listings.
 *
 * This pins the retry half: a transient failure that recovers within
 * buildFetch's attempt budget must still populate the sitemap, not
 * silently degrade to zero rows for that family.
 */

const ISSUER_ACCOUNT = 'GATESTSITEMAPXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXX'; // gitleaks:allow — fake fixture shape, not a real Stellar account

function jsonResponse(rows: unknown): Response {
  return new Response(JSON.stringify({ data: rows }), {
    status: 200,
    headers: { 'content-type': 'application/json' },
  });
}

afterEach(() => {
  vi.unstubAllGlobals();
  vi.resetModules();
});

describe('sitemap issuer listing', () => {
  it('recovers from one transient fetch failure instead of permanently dropping the issuer pages', async () => {
    let issuerAttempts = 0;
    const fetchMock = vi.fn(async (input: RequestInfo | URL) => {
      const url = String(input);
      if (url.includes('/v1/issuers')) {
        issuerAttempts++;
        if (issuerAttempts === 1) {
          throw new Error('simulated transient network failure');
        }
        return jsonResponse([{ g_strkey: ISSUER_ACCOUNT }]);
      }
      if (url.includes('/v1/sources')) {
        return jsonResponse([
          { name: 'sdex', class: 'exchange', subclass: 'dex' },
        ]);
      }
      if (url.includes('/v1/lending/pools')) return jsonResponse([]);
      if (url.includes('/v1/markets')) {
        return jsonResponse([{ base: 'native', quote: 'USDC-ISSUER' }]);
      }
      if (url.includes('/v1/assets/verified')) {
        return jsonResponse([{ ticker: 'USD', class: 'fiat' }]);
      }
      if (url.includes('/v1/assets')) return jsonResponse([{ slug: 'xlm' }]);
      return jsonResponse([]);
    });
    vi.stubGlobal('fetch', fetchMock);

    const sitemap = (await import('./sitemap')).default;
    const entries = await sitemap();

    // The first attempt failed — proves this isn't a same-call fluke, it
    // exercised an actual retry.
    expect(issuerAttempts).toBeGreaterThan(1);
    expect(
      entries.some((e) => e.url.includes(`/issuers/${ISSUER_ACCOUNT}/`)),
    ).toBe(true);
  });
});

describe('sitemap venue pages', () => {
  it('lists exactly the pre-rendered /exchanges and /dexes pages, whatever /v1/sources says', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn(async (input: RequestInfo | URL) => {
        const url = String(input);
        if (url.includes('/v1/issuers')) {
          return jsonResponse([{ g_strkey: ISSUER_ACCOUNT }]);
        }
        // An unmapped cex + dex (no page built) and none of the mapped venues.
        if (url.includes('/v1/sources')) {
          return jsonResponse([
            { name: 'unmapped-cex', class: 'exchange', subclass: 'cex' },
            { name: 'unmapped-dex', class: 'exchange', subclass: 'dex' },
          ]);
        }
        if (url.includes('/v1/lending/pools')) return jsonResponse([]);
        if (url.includes('/v1/markets')) {
          return jsonResponse([{ base: 'native', quote: 'USDC-ISSUER' }]);
        }
        if (url.includes('/v1/assets/verified')) {
          return jsonResponse([{ ticker: 'USD', class: 'fiat' }]);
        }
        if (url.includes('/v1/assets')) return jsonResponse([{ slug: 'xlm' }]);
        return jsonResponse([]);
      }),
    );

    const sitemap = (await import('./sitemap')).default;
    const exchangePage = await import('./exchanges/[name]/page');
    const dexPage = await import('./dexes/[source]/page');
    const paths = (await sitemap()).map((e) => new URL(e.url).pathname);

    // Child pages only: the hub itself is `/<family>/`.
    const listed = (family: string) =>
      paths
        .filter((p) => p.startsWith(`/${family}/`) && p !== `/${family}/`)
        .map((p) => p.slice(family.length + 2, -1))
        .sort();
    expect(listed('exchanges')).toEqual(
      exchangePage
        .generateStaticParams()
        .map((p) => p.name)
        .sort(),
    );
    expect(listed('dexes')).toEqual(
      dexPage
        .generateStaticParams()
        .map((p) => p.source)
        .sort(),
    );
    expect(paths).toContain('/sources/unmapped-cex/');
  });
});
