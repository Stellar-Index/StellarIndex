import {
  mkdtempSync,
  readdirSync,
  readFileSync,
  rmSync,
  writeFileSync,
} from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import {
  buildBakedDegraded,
  buildFetchBatchedPrice,
  buildFetchData,
  buildFetchEnvelope,
  buildFetchPriceBatch,
  buildReaskCounts,
  buildRequestCounts,
  DEGRADED_REASK_DELAYS_MS,
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

describe('price batch gate', () => {
  afterEach(() => {
    vi.unstubAllGlobals();
  });

  it('runs batches one at a time inside the global cap while other routes overlap', async () => {
    let cur = 0;
    let peak = 0;
    let curBatch = 0;
    let peakBatch = 0;
    let othersDuringBatch = 0;
    vi.stubGlobal(
      'fetch',
      vi.fn(async (url: string) => {
        const isBatch = url.includes('/v1/price/batch');
        cur++;
        peak = Math.max(peak, cur);
        if (isBatch) {
          curBatch++;
          peakBatch = Math.max(peakBatch, curBatch);
        }
        if (curBatch > 0) {
          othersDuringBatch = Math.max(othersDuringBatch, cur - curBatch);
        }
        await new Promise((r) => setTimeout(r, 5));
        cur--;
        if (isBatch) curBatch--;
        return fakeResponse(200, { data: isBatch ? ['b'] : 1 });
      }),
    );
    const batches = Array.from({ length: 6 }, (_, i) =>
      buildFetchData<string[]>(`/v1/price/batch?asset_ids=gate${i}&quote=fiat:USD`),
    );
    const others = Array.from({ length: 12 }, (_, i) =>
      buildFetchData<number>(`/v1/gate-other/${i}`),
    );
    const [b, o] = await Promise.all([Promise.all(batches), Promise.all(others)]);
    expect(b.every((r) => r?.[0] === 'b')).toBe(true);
    expect(o.every((r) => r === 1)).toBe(true);
    expect(peakBatch).toBe(1);
    expect(peak).toBeLessThanOrEqual(MAX_IN_FLIGHT);
    expect(othersDuringBatch).toBeGreaterThan(1);
  });

  it('releases the batch slot when a request fails', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn(async (url: string) => {
        if (url.includes('gatefail')) throw new Error('boom');
        return fakeResponse(200, { data: ['ok'] });
      }),
    );
    const failing = buildFetchData('/v1/price/batch?asset_ids=gatefail', {
      attempts: 1,
    });
    const next = buildFetchData<string[]>('/v1/price/batch?asset_ids=gateok');
    await expect(failing).rejects.toThrow(/boom/);
    expect(await next).toEqual(['ok']);
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

  describe('across the build passes', () => {
    // The production build's slow batches: the /v1/assets listing's 500 ids in
    // USD (5 chunks) and the XLM-quoted market bases (2 chunks), each sent
    // again by `next build`'s second render pass in fresh worker processes.
    const assetIds = Array.from({ length: 500 }, (_, i) => `crypto:A${i}`);
    const markets = [
      ...Array.from({ length: 180 }, (_, i) => ({ base: `crypto:M${i}`, quote: 'native' })),
      { base: 'native', quote: 'fiat:USD' },
    ];
    let dir = '';
    let calls: string[] = [];
    let failBatches = false;

    // One worker process: a fresh module (empty in-memory memo, own nonce).
    async function worker(nonceMs: number) {
      vi.resetModules();
      const now = vi.spyOn(Date, 'now').mockReturnValue(nonceMs);
      const mod = await import('./buildFetch');
      now.mockRestore();
      const usd = await mod.buildFetchPriceBatch(assetIds, 'fiat:USD');
      const pair = await mod.buildFetchPairPrice('crypto:M7', 'native');
      const xlm = await mod.buildFetchPairPrice('native', 'fiat:USD');
      return { usd, pair, xlm };
    }
    const batchCalls = () => {
      const n = new Map<string, number>();
      for (const u of calls.filter((c) => c.includes('/v1/price/batch'))) {
        const url = new URL(u);
        url.searchParams.delete('b');
        n.set(url.toString(), (n.get(url.toString()) ?? 0) + 1);
      }
      return n;
    };

    beforeEach(() => {
      dir = mkdtempSync(join(tmpdir(), 'bf-batch-'));
      calls = [];
      failBatches = false;
      vi.spyOn(process, 'cwd').mockReturnValue(dir);
      vi.stubEnv('NEXT_PHASE', 'phase-production-build');
      vi.stubGlobal(
        'fetch',
        vi.fn(async (url: string) => {
          calls.push(url);
          if (url.includes('/v1/markets')) return fakeResponse(200, { data: markets });
          if (failBatches) return fakeResponse(400);
          const ids = new URL(url).searchParams.get('asset_ids')!.split(',');
          return fakeResponse(200, {
            // A0 is missing (omitted row); A1 is stale; prices vary per request.
            data: ids
              .filter((id) => id !== 'crypto:A0')
              .map((id) => ({ asset_id: id, price: `${calls.length}.25` })),
            stale: ids.filter((id) => id === 'crypto:A1'),
            triangulated: [],
            flags: { stale: ids.includes('crypto:A1') },
          });
        }),
      );
    });
    afterEach(() => {
      vi.restoreAllMocks();
      vi.unstubAllEnvs();
      rmSync(dir, { recursive: true, force: true });
    });

    it('prices each chunk once per build and renders the same values and flags', async () => {
      const first = await worker(1);
      const second = await worker(2);
      const n = batchCalls();
      // 5 USD chunks + 2 XLM-quoted market chunks + 1 USD market chunk, once each.
      expect(n.size).toBe(8);
      expect([...n.values()].every((c) => c === 1)).toBe(true);
      expect(second).toEqual(first);
      expect(first.usd.get('crypto:A0')).toBeNull();
      expect(first.usd.get('crypto:A1')?.stale).toBe(true);
      expect(first.usd.get('crypto:A2')?.stale).toBe(false);
      expect(first.xlm?.asset_id).toBe('native');
      // The second worker has its own nonce yet sent nothing: the memo key has none.
      const nonces = new Set(
        calls
          .filter((c) => c.includes('/v1/price/batch'))
          .map((c) => new URL(c).searchParams.get('b')),
      );
      expect(nonces).toEqual(new Set([(1).toString(36)]));
      for (const f of readdirSync(join(dir, '.next', 'build-price-batch'))) {
        const saved = readFileSync(join(dir, '.next', 'build-price-batch', f), 'utf8');
        expect(saved).not.toContain('b=');
      }
    });

    it('ignores a corrupt memo file and never persists a failed chunk', async () => {
      await worker(1);
      const memoDir = join(dir, '.next', 'build-price-batch');
      const files = readdirSync(memoDir);
      for (const f of files) writeFileSync(join(memoDir, f), '{"path":"/v1/pri');
      const second = await worker(2);
      expect([...batchCalls().values()].every((c) => c === 2)).toBe(true);
      expect(second.usd.get('crypto:A1')?.stale).toBe(true);

      rmSync(memoDir, { recursive: true, force: true });
      failBatches = true;
      expect((await worker(3)).usd.size).toBe(0);
      failBatches = false;
      calls = [];
      expect((await worker(4)).usd.size).toBe(500);
      expect(batchCalls().size).toBe(8);
    });
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

// The API marks a partial 200 only with `Cache-Control: no-store`.
function answer(body: unknown, cacheControl: string) {
  return {
    status: 200,
    ok: true,
    headers: {
      get: (h: string) => (h.toLowerCase() === 'cache-control' ? cacheControl : null),
    },
    json: async () => body,
  };
}

describe('degraded core answers', () => {
  beforeEach(() => {
    vi.useFakeTimers();
  });
  afterEach(() => {
    vi.useRealTimers();
    vi.unstubAllGlobals();
  });

  async function settle<T>(p: Promise<T>): Promise<T> {
    await vi.runAllTimersAsync();
    return p;
  }

  it('re-asks a degraded detail and bakes the complete answer', async () => {
    const replies = [
      answer({ data: { ath: null } }, 'no-store'),
      answer({ data: { ath: '1.5' } }, 'public, max-age=30'),
    ];
    const fetchMock = vi.fn(async () => replies.shift()!);
    vi.stubGlobal('fetch', fetchMock);
    const path = '/v1/assets/DGR1-GAAA';
    const reasksBefore = buildReaskCounts().get('/v1/assets/{id}') ?? 0;
    const env = await settle(buildFetchEnvelope<{ ath: string | null }>(path));
    expect(env.data).toEqual({ ath: '1.5' });
    expect(env.degraded).toBeUndefined();
    expect(fetchMock).toHaveBeenCalledTimes(2);
    expect(buildReaskCounts().get('/v1/assets/{id}')).toBe(reasksBefore + 1);
    expect(buildBakedDegraded().has(path)).toBe(false);
    // The memo holds the final answer.
    expect((await buildFetchEnvelope(path)).data).toEqual({ ath: '1.5' });
    expect(fetchMock).toHaveBeenCalledTimes(2);
  });

  it('returns a persistently degraded answer marked and lists it', async () => {
    const fetchMock = vi.fn(async () => answer({ data: { ath: null } }, 'no-store'));
    vi.stubGlobal('fetch', fetchMock);
    const path = '/v1/assets/DGR2-GAAA';
    const env = await settle(buildFetchEnvelope(path));
    expect(env.degraded).toBe(true);
    expect(env.data).toEqual({ ath: null });
    expect(fetchMock).toHaveBeenCalledTimes(
      1 + DEGRADED_REASK_DELAYS_MS.get('/v1/assets/{id}')!.length,
    );
    expect(buildBakedDegraded().has(path)).toBe(true);
  });

  it('never re-asks a complete answer, nor a no-store outside the core routes', async () => {
    const fetchMock = vi.fn(async (url: string) =>
      answer({ data: [] }, url.includes('/v1/pools') ? 'public, max-age=30' : 'no-store'),
    );
    vi.stubGlobal('fetch', fetchMock);
    await settle(buildFetchEnvelope('/v1/pools?asset=DGR3'));
    await settle(buildFetchEnvelope('/v1/price/batch?asset_ids=DGR3&quote=fiat:USD'));
    expect(fetchMock).toHaveBeenCalledTimes(2);
  });

  it('re-asks inside the in-flight cap', async () => {
    let current = 0;
    let peak = 0;
    const seen = new Map<string, number>();
    vi.stubGlobal(
      'fetch',
      vi.fn(async (url: string) => {
        current++;
        peak = Math.max(peak, current);
        await new Promise((r) => setTimeout(r, 5));
        current--;
        const key = url.split('&b=')[0];
        const n = (seen.get(key) ?? 0) + 1;
        seen.set(key, n);
        return answer({ data: [] }, n === 1 ? 'no-store' : 'public, max-age=30');
      }),
    );
    const all = Promise.all(
      Array.from({ length: MAX_IN_FLIGHT * 3 }, (_, i) =>
        buildFetchEnvelope(`/v1/pools?asset=DGRCAP${i}`),
      ),
    );
    const envs = await settle(all);
    expect(envs.every((e) => Array.isArray(e.data) && !e.degraded)).toBe(true);
    expect([...seen.values()].every((n) => n === 2)).toBe(true);
    expect(peak).toBeLessThanOrEqual(MAX_IN_FLIGHT);
  });
});
