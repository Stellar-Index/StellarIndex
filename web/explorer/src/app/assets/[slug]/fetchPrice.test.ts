// @vitest-environment node
//
// fetchPriceDirect/fetchPrice are build-time (Node) data fetchers for the
// static-export /assets/[slug] page — no DOM involved.
import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';

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
    let call = 0;
    vi.stubGlobal(
      'fetch',
      vi.fn(async () => {
        call += 1;
        // First direct-quote attempt 404s (forces triangulation); the two
        // triangulation legs follow, one of them stale.
        if (call === 1) {
          return new Response('not found', { status: 404 });
        }
        const stale = call === 2; // asset/native leg is stale
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
