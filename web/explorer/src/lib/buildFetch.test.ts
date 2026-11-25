import { readFileSync } from 'node:fs';
import { join } from 'node:path';

import { afterEach, describe, expect, it, vi } from 'vitest';

import {
  buildFetchBatchedPrice,
  buildFetchData,
  buildFetchPriceBatch,
  buildRequestCounts,
  MAX_IN_FLIGHT,
  MAX_UNAVAILABLE_WAITS,
  routeTemplate,
  throttleDelayMs,
} from './buildFetch';

function fakeResponse(status: number, body: unknown = {}) {
  return {
    status,
    ok: status >= 200 && status < 300,
    headers: { get: () => null },
    json: async () => body,
  };
}

// A 429 is the server asking us to slow down, not a transport failure. These
// pin the wait policy: one launch-rehearsal export died on HTTP 429 from our
// own rate limiter after burning its 5 transport attempts in ~10s of linear
// backoff (it passed on retry — the tell that nothing was actually broken, we
// just asked too fast).
describe('throttleDelayMs', () => {
  it('prefers a numeric Retry-After, in seconds', () => {
    expect(throttleDelayMs('2', 0)).toBe(2_000);
    expect(throttleDelayMs('0', 3)).toBe(0);
  });

  it('accepts an HTTP-date Retry-After', () => {
    const inTen = new Date(Date.now() + 10_000).toUTCString();
    const got = throttleDelayMs(inTen, 0);
    // Second-granularity date parsing, so allow a little slack.
    expect(got).toBeGreaterThan(8_000);
    expect(got).toBeLessThanOrEqual(10_000);
  });

  it('treats an already-elapsed Retry-After date as no wait', () => {
    const past = new Date(Date.now() - 5_000).toUTCString();
    expect(throttleDelayMs(past, 0)).toBe(0);
  });

  it('caps a hostile or misconfigured Retry-After', () => {
    // Without a cap this stalls a build for hours.
    expect(throttleDelayMs('86400', 0)).toBe(60_000);
  });

  it('ignores an unparseable Retry-After and falls back to backoff', () => {
    const got = throttleDelayMs('soon-ish', 0);
    expect(got).toBeGreaterThanOrEqual(1_000);
    expect(got).toBeLessThan(1_600);
  });

  it('backs off exponentially, capped, when no Retry-After is given', () => {
    expect(throttleDelayMs(null, 0)).toBeGreaterThanOrEqual(1_000);
    expect(throttleDelayMs(null, 1)).toBeGreaterThanOrEqual(2_000);
    expect(throttleDelayMs(null, 2)).toBeGreaterThanOrEqual(4_000);
    expect(throttleDelayMs(null, 3)).toBeGreaterThanOrEqual(8_000);
    // Capped — an unbounded 2**n would reach hours by the 8th wait.
    expect(throttleDelayMs(null, 20)).toBeLessThanOrEqual(30_500);
  });

  it('jitters the fallback so fanned-out pages do not resynchronise', () => {
    const samples = new Set(
      Array.from({ length: 40 }, () => throttleDelayMs(null, 0)),
    );
    expect(samples.size).toBeGreaterThan(1);
  });

  it('waits long enough in total to outlast a rate-limit window', () => {
    // 8 waits of capped exponential backoff — the budget the fetch loop
    // allows. Must comfortably exceed the anonymous tier's fixed window,
    // otherwise a build dies inside the very window it is waiting out.
    const total = Array.from({ length: 8 }, (_, i) =>
      throttleDelayMs(null, i),
    ).reduce((a, b) => a + b, 0);
    expect(total).toBeGreaterThan(90_000);
  });
});

// A 502/503/504 is the API "temporarily unavailable" — typically mid-deploy,
// its reverse proxy answering with no upstream. The build waits it out on the
// SAME budget mechanism as a 429 (throttleDelayMs, without spending a transport
// attempt). A single /v1/assets/LUKOIL-…
// 503 racing an API deploy must not fail the explorer deploy; a
// 5×`500*attempt` path gives up in ~5s. These pin the budget so it can't silently regress below an
// API-restart window — nor grow into an unbounded CI hang.
describe('502/503/504 unavailable-wait budget', () => {
  it('waits long enough in total to outlast an API-restart window', () => {
    const total = Array.from({ length: MAX_UNAVAILABLE_WAITS }, (_, i) =>
      throttleDelayMs(null, i),
    ).reduce((a, b) => a + b, 0);
    // A rolling API restart is seconds-to-tens-of-seconds; the budget must
    // comfortably clear that or the export dies inside the very window it
    // waits out (exactly the LUKOIL 503 failure).
    expect(total).toBeGreaterThan(45_000);
  });

  it('stays bounded so a sustained outage still fails-hard', () => {
    const total = Array.from({ length: MAX_UNAVAILABLE_WAITS }, (_, i) =>
      throttleDelayMs(null, i),
    ).reduce((a, b) => a + b, 0);
    // Patience, not infinity: a genuinely dead API must fail the build
    // promptly (keeping the last good deploy live), not hang CI for minutes.
    expect(total).toBeLessThan(180_000);
  });
});

// The API is unauthenticated (AGENTS.md — "public REST +
// SSE API"), so a 401/403 can only be an intermediary (edge/WAF) response,
// never the origin API's own "not found". Collapsing it into the same
// null bucket as a real 404 bakes a not-found page over a transport
// failure. A softFail caller (build-time live-price enrichment) still
// degrades to null; a fail-hard caller must see the failure instead.
describe('non-authoritative 4xx (e.g. 403) handling', () => {
  afterEach(() => {
    vi.unstubAllGlobals();
  });

  it('resolves null for a softFail caller', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn().mockResolvedValue(fakeResponse(403, { title: 'blocked' })),
    );
    const result = await buildFetchData('/v1/price/ca2-a35-softfail', {
      softFail: true,
      attempts: 1,
    });
    expect(result).toBeNull();
  });

  it('throws for a fail-hard (non-softFail) caller, carrying the problem title', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn().mockResolvedValue(fakeResponse(403, { title: 'blocked by WAF' })),
    );
    await expect(
      buildFetchData('/v1/entity/ca2-a35-hardfail', { attempts: 1 }),
    ).rejects.toThrow(/blocked by WAF/);
  });
});

describe('request counter', () => {
  afterEach(() => {
    vi.unstubAllGlobals();
  });

  it('counts each request sent per route template, not memo hits', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn(async () => fakeResponse(200, { data: [] })),
    );
    const before = new Map(buildRequestCounts());
    const delta = (route: string) =>
      (buildRequestCounts().get(route) ?? 0) - (before.get(route) ?? 0);
    await buildFetchData(`/v1/assets/CNT1-G${'A'.repeat(55)}`);
    await buildFetchData('/v1/assets/cnt2');
    await buildFetchData('/v1/assets/cnt2'); // memo hit: no request
    await buildFetchData('/v1/assets/verified?cnt=1');
    await buildFetchData('/v1/price/batch?asset_ids=crypto:CNT&quote=fiat:USD');
    await buildFetchData('/v1/lending/pools/CCNT/reserves');
    expect(delta('/v1/assets/{id}')).toBe(2);
    expect(delta('/v1/assets/verified')).toBe(1);
    expect(delta('/v1/price/batch')).toBe(1);
    expect(delta('/v1/lending/pools/{id}/reserves')).toBe(1);
  });

  // An OpenAPI word missing from ROUTE_WORDS would read as an id and fold
  // a route into its sibling's line.
  it('templates every OpenAPI path to itself', () => {
    const repo = join(__dirname, '..', '..', '..', '..');
    const spec = readFileSync(join(repo, 'openapi', 'stellar-index.v1.yaml'));
    const paths = String(spec)
      .split('\n')
      .map((line) => /^ {2}(\/\S+):\s*$/.exec(line)?.[1])
      .filter((p): p is string => p !== undefined);
    expect(paths.length).toBeGreaterThan(100);
    for (const p of paths) {
      const want = `/v1${p.replace(/\{[^}]+\}/g, '{id}')}`;
      expect(routeTemplate(`/v1${p.replace(/\{[^}]+\}/g, 'x1')}`)).toBe(want);
    }
  });
});

describe('in-flight cap', () => {
  afterEach(() => {
    vi.unstubAllGlobals();
  });

  it('never has more than MAX_IN_FLIGHT requests outstanding', async () => {
    let current = 0;
    let peak = 0;
    vi.stubGlobal(
      'fetch',
      vi.fn(async () => {
        current++;
        peak = Math.max(peak, current);
        await new Promise((r) => setTimeout(r, 5));
        current--;
        return fakeResponse(200, { data: 1 });
      }),
    );
    const results = await Promise.all(
      Array.from({ length: MAX_IN_FLIGHT * 5 }, (_, i) =>
        buildFetchData<number>(`/v1/cap-test/${i}`),
      ),
    );
    expect(results.every((r) => r === 1)).toBe(true);
    expect(peak).toBe(MAX_IN_FLIGHT);
  });
});

describe('buildFetchPriceBatch', () => {
  afterEach(() => {
    vi.unstubAllGlobals();
  });

  it('chunks at the GET cap, dedupes, and maps unanswered ids to null', async () => {
    const urls: string[] = [];
    vi.stubGlobal(
      'fetch',
      vi.fn(async (url: string) => {
        urls.push(url);
        const ids = new URL(url).searchParams.get('asset_ids')!.split(',');
        return fakeResponse(200, {
          data: ids
            .filter((id) => id !== 'crypto:CHUNK7')
            .map((id) => ({ asset_id: id, price: '1' })),
          stale: ['crypto:CHUNK3'],
          flags: { stale: true },
        });
      }),
    );
    const ids = Array.from({ length: 150 }, (_, i) => `crypto:CHUNK${i}`);
    const got = await buildFetchPriceBatch([...ids, ...ids, 'fiat:USD'], 'fiat:USD');
    expect(urls).toHaveLength(2);
    expect(got.get('crypto:CHUNK7')).toBeNull();
    expect(got.get('crypto:CHUNK3')?.stale).toBe(true);
    expect(got.get('crypto:CHUNK4')?.stale).toBe(false);
    expect(got.has('fiat:USD')).toBe(false);
  });

  it('serves an id from the universe batch and falls back to one request when its chunk is rejected', async () => {
    const urls: string[] = [];
    vi.stubGlobal(
      'fetch',
      vi.fn(async (url: string) => {
        urls.push(url);
        const ids = new URL(url).searchParams.get('asset_ids')!.split(',');
        // The batch 400s a whole request on one id it cannot parse.
        if (ids.length > 1 && ids.includes('crypto:FB99'))
          return fakeResponse(400);
        return fakeResponse(200, {
          data: ids.map((id) => ({ asset_id: id, price: `${id.length}.5` })),
          stale: [],
          triangulated: [],
        });
      }),
    );
    // Sorted, FB3 and FB150 land in the first 100-id chunk, FB99 in the second.
    const universe = Array.from({ length: 160 }, (_, i) => `crypto:FB${i}`);
    const price = async (id: string) =>
      (await buildFetchBatchedPrice(id, 'fiat:EUR', universe))?.price;
    expect(await price('crypto:FB3')).toBe('10.5');
    expect(await price('crypto:FB150')).toBe('12.5');
    expect(urls).toHaveLength(2); // both chunks, once
    // FB99 sits in the rejected chunk: one one-id request, the same row.
    expect(await price('crypto:FB99')).toBe('11.5');
    // Outside the universe: one one-id request.
    expect(await price('crypto:OUT')).toBe('10.5');
    expect(urls).toHaveLength(4);
    expect(new URL(urls[3]).searchParams.get('asset_ids')).toBe('crypto:OUT');
  });

  it('leaves a row flag undefined when the OR flag is set without the id list', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn(async () =>
        fakeResponse(200, {
          data: [{ asset_id: 'crypto:OLDAPI', price: '1' }],
          flags: { triangulated: true },
        }),
      ),
    );
    const got = await buildFetchPriceBatch(['crypto:OLDAPI'], 'fiat:USD');
    expect(got.get('crypto:OLDAPI')?.triangulated).toBeUndefined();
    expect(got.get('crypto:OLDAPI')?.stale).toBe(false);
  });
});
