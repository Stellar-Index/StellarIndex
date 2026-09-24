// @vitest-environment node
//
// fetchPriceDirect/fetchPrice are build-time (Node) data fetchers for the
// static-export /assets/[slug] page — no DOM involved.
import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';

// Budget, not speed. This file's cost is ONE `await import('./page')` — the
// static-export page module and its whole Next.js dependency graph, resolved
// and transformed once. The assertions after it take milliseconds.
//
// Measured on an idle machine the file runs in ~750 ms, comfortably inside
// vitest's 5 s default. Under a loaded one it does not: this test timed out
// three times in a single session while deploys, an SSH session and a
// container gate shared the box, each time passing in under a second when
// re-run alone. A 5 s budget on a module import is a load sensor, not a
// correctness check, and a gate that goes red for the machine teaches people
// to re-run red gates instead of reading them.
//
// 30 s is ~40x the idle cost, so a genuine hang still fails.
vi.setConfig({ testTimeout: 30_000 });

function envelopeResponse(body: Record<string, unknown>) {
  return new Response(JSON.stringify(body), {
    status: 200,
    headers: { 'content-type': 'application/json' },
  });
}

// buildFetch.ts memoizes per-URL for the module's lifetime (by design —
// it's meant to persist for one `next build` run). Reset the module
// registry between tests so each test gets a fresh memo instead of
// silently reusing another test's cached /v1/price?asset=native&
// quote=fiat:USD response (both tests exercise that exact URL).
beforeEach(() => {
  vi.resetModules();
});
afterEach(() => {
  vi.restoreAllMocks();
});

describe('fetchPriceDirect / fetchPrice — AGT-06 real flags.stale propagation', () => {
  it("fetchPriceDirect surfaces the envelope's real flags.stale (not permanently false)", async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn(async () =>
        envelopeResponse({
          data: { price: '1.2345', quote: 'fiat:USD' },
          as_of: new Date().toISOString(),
          flags: {
            stale: true,
            reduced_redundancy: false,
            triangulated: false,
            divergence_warning: false,
            divergence_checked: false,
          },
        }),
      ),
    );
    const { fetchPriceDirect } = await import('./page');
    const result = await fetchPriceDirect('native', 'fiat:USD');
    expect(result?.flags?.stale).toBe(true);
  });

  it('a triangulated price is marked stale when either leg is stale', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn(async (input: RequestInfo | URL) => {
        // Keyed on the REQUEST, never on arrival order: fetchPrice races the
        // two triangulation legs through Promise.all, so a counter marks
        // whichever leg's continuation happens to run first. Under load that
        // is the wrong one, and the test fails claiming the fix regressed it.
        const url = String(input instanceof Request ? input.url : input);
        const q = new URL(url, 'http://x').searchParams;
        const asset = q.get('asset') ?? '';
        const quote = q.get('quote') ?? '';
        // The direct asset->USD quote 404s, which is what forces triangulation.
        if (quote === 'fiat:USD' && asset !== 'native') {
          return new Response('not found', { status: 404 });
        }
        const stale = quote === 'native'; // the asset/native leg is the stale one
        return envelopeResponse({
          data: { price: '2.0', quote: 'native' },
          as_of: new Date().toISOString(),
          flags: {
            stale,
            reduced_redundancy: false,
            triangulated: false,
            divergence_warning: false,
            divergence_checked: false,
          },
        });
      }),
    );
    const { fetchPrice } = await import('./page');
    const result = await fetchPrice('SOME-ASSET');
    expect(result?.flags?.triangulated).toBe(true);
    expect(result?.flags?.stale).toBe(true);
  });
});

// Serves `legs[quote]` as the price of each triangulation leg and 404s the
// direct asset->USD quote, keyed on the request (see the stale test above).
function triangulationFetch(legs: { native: string; usd: string }) {
  return vi.fn(async (input: RequestInfo | URL) => {
    const url = String(input instanceof Request ? input.url : input);
    const q = new URL(url, 'http://x').searchParams;
    const asset = q.get('asset') ?? '';
    const quote = q.get('quote') ?? '';
    if (quote === 'fiat:USD' && asset !== 'native') {
      return new Response('not found', { status: 404 });
    }
    return envelopeResponse({
      data: { price: quote === 'native' ? legs.native : legs.usd, quote },
      as_of: new Date().toISOString(),
      flags: { stale: false, triangulated: false },
    });
  });
}

describe('fetchPrice — triangulated product keeps its value', () => {
  it('a sub-5e-13 product is served exactly, not rounded to a zero price', async () => {
    vi.stubGlobal(
      'fetch',
      triangulationFetch({ native: '0.000000001', usd: '0.0003' }),
    );
    const { fetchPrice } = await import('./page');
    const result = await fetchPrice('SOME-ASSET');
    expect(result?.flags?.triangulated).toBe(true);
    expect(result?.price).toBe('0.0000000000003');
  });

  it('multiplies the served decimal strings exactly', async () => {
    vi.stubGlobal(
      'fetch',
      triangulationFetch({ native: '0.1234567891234567', usd: '0.3' }),
    );
    const { fetchPrice } = await import('./page');
    const result = await fetchPrice('SOME-ASSET');
    expect(result?.price).toBe('0.03703703673703701');
  });

  it('withholds the compose when a leg is not a plain decimal', async () => {
    vi.stubGlobal('fetch', triangulationFetch({ native: '1e-9', usd: '0.3' }));
    const { fetchPrice } = await import('./page');
    expect(await fetchPrice('SOME-ASSET')).toBeNull();
  });
});
