import { mkdirSync, mkdtempSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

type Mod = typeof import('./buildFetch');

// The API marks a partial 200 only with `Cache-Control: no-store`.
function answer(body: unknown, partial: boolean) {
  return {
    status: 200,
    ok: true,
    headers: {
      get: (h: string) =>
        h.toLowerCase() === 'cache-control'
          ? partial
            ? 'no-store'
            : 'public, max-age=30'
          : null,
    },
    json: async () => body,
  };
}

const keyOf = (url: string) =>
  url.split(/[?&]b=/)[0].replace(/^https?:\/\/[^/]+/, '');

// Every test gets a fresh module: the budget, streaks and memo are per process.
async function fresh(): Promise<Mod> {
  vi.resetModules();
  return import('./buildFetch');
}

describe('re-ask bounds', () => {
  let calls: string[];
  beforeEach(() => {
    vi.useFakeTimers();
    calls = [];
  });
  afterEach(() => {
    vi.useRealTimers();
    vi.unstubAllGlobals();
    vi.unstubAllEnvs();
    vi.restoreAllMocks();
  });

  function api(partial: (key: string, nth: number) => boolean) {
    const seen = new Map<string, number>();
    vi.stubGlobal(
      'fetch',
      vi.fn(async (url: string) => {
        calls.push(url);
        const key = keyOf(url);
        const nth = (seen.get(key) ?? 0) + 1;
        seen.set(key, nth);
        return answer(
          { data: key.startsWith('/v1/pools') ? [] : { id: key } },
          partial(key, nth),
        );
      }),
    );
  }

  it('re-asks asset detail once after 4 s and bakes the healed answer', async () => {
    const m = await fresh();
    api((_k, nth) => nth === 1);
    const p = m.buildFetchEnvelope('/v1/assets/HEAL-GAAA');
    await vi.advanceTimersByTimeAsync(3_999);
    expect(calls).toHaveLength(1);
    await vi.advanceTimersByTimeAsync(1);
    const env = await p;
    expect(calls).toHaveLength(2);
    expect(env.degraded).toBeUndefined();
    expect(m.buildBakedDegraded().size).toBe(0);
    expect(m.buildReaskSleptMs()).toBe(4_000);
  });

  it('re-asks pools after 1.5 s then 4 s, and no more', async () => {
    const m = await fresh();
    api(() => true);
    const p = m.buildFetchEnvelope('/v1/pools?asset=P1');
    await vi.advanceTimersByTimeAsync(1_500);
    expect(calls).toHaveLength(2);
    await vi.advanceTimersByTimeAsync(4_000);
    const env = await p;
    await vi.runAllTimersAsync();
    expect(calls).toHaveLength(3);
    expect(env.degraded).toBe(true);
    expect(m.buildBakedDegraded().has('/v1/pools?asset=P1')).toBe(true);
  });

  it('stops sleeping once the build budget is spent and bakes the rest partial, listed', async () => {
    const m = await fresh();
    api(() => true);
    const n = 200;
    const all = Promise.all(
      Array.from({ length: n }, (_, i) =>
        m.buildFetchEnvelope(`/v1/assets/BUD${i}-GAAA`),
      ),
    );
    await vi.runAllTimersAsync();
    const envs = await all;
    const reasks = m.REASK_BUDGET_MS / 4_000;
    expect(m.buildReaskSleptMs()).toBe(m.REASK_BUDGET_MS);
    expect(calls).toHaveLength(n + reasks);
    expect(envs.every((e) => e.degraded === true)).toBe(true);
    expect(m.buildBakedDegraded().size).toBe(n);
  });

  it('counts the budget other build processes already spent', async () => {
    const dir = mkdtempSync(join(tmpdir(), 'bf-reask-'));
    try {
      vi.spyOn(process, 'cwd').mockReturnValue(dir);
      vi.stubEnv('NEXT_PHASE', 'phase-production-build');
      const m = await fresh();
      const counts = join(dir, '.next', 'build-fetch-counts');
      mkdirSync(counts, { recursive: true });
      writeFileSync(
        join(counts, '1.json'),
        JSON.stringify({ reaskSleptMs: m.REASK_BUDGET_MS - 3_000 }),
      );
      api(() => true);
      const env = m.buildFetchEnvelope('/v1/assets/SIB-GAAA');
      await vi.runAllTimersAsync();
      expect((await env).degraded).toBe(true);
      expect(calls).toHaveLength(1);
      expect(m.buildReaskSleptMs()).toBe(0);
    } finally {
      rmSync(dir, { recursive: true, force: true });
    }
  });

  it('disables a route after REASK_UNHEALED_LIMIT unhealed re-asks in a row', async () => {
    const m = await fresh();
    api(() => true);
    for (let i = 0; i < m.REASK_UNHEALED_LIMIT + 3; i++) {
      const p = m.buildFetchEnvelope(`/v1/assets/RPL${i}-GAAA`);
      await vi.runAllTimersAsync();
      expect((await p).degraded).toBe(true);
    }
    expect(m.buildReaskDisabled().has('/v1/assets/{id}')).toBe(true);
    expect(m.buildReaskSleptMs()).toBe(m.REASK_UNHEALED_LIMIT * 4_000);
    expect(calls).toHaveLength(2 * m.REASK_UNHEALED_LIMIT + 3);
    expect(m.buildBakedDegraded().size).toBe(m.REASK_UNHEALED_LIMIT + 3);
    // Another route keeps its re-asks.
    const pools = m.buildFetchEnvelope('/v1/pools?asset=OTHER');
    await vi.runAllTimersAsync();
    await pools;
    expect(calls.filter((c) => c.includes('/v1/pools'))).toHaveLength(3);
  });

  it('a healed re-ask resets the unhealed streak', async () => {
    const m = await fresh();
    const healing = new Set(['/v1/assets/H-GAAA']);
    api((key, nth) => nth === 1 || !healing.has(key));
    const ask = async (path: string) => {
      const p = m.buildFetchEnvelope(path);
      await vi.runAllTimersAsync();
      return p;
    };
    for (let i = 0; i < m.REASK_UNHEALED_LIMIT - 1; i++)
      await ask(`/v1/assets/A${i}-GAAA`);
    expect((await ask('/v1/assets/H-GAAA')).degraded).toBeUndefined();
    for (let i = 0; i < m.REASK_UNHEALED_LIMIT - 1; i++)
      await ask(`/v1/assets/B${i}-GAAA`);
    expect(m.buildReaskDisabled().size).toBe(0);
  });

  it('simulated mainnet build: 2,000 asset pages against a 120 s partial replay stay within budget', async () => {
    const m = await fresh();
    const firstAsk = new Map<string, number>();
    api((key) => {
      if (!firstAsk.has(key)) firstAsk.set(key, Date.now());
      return Date.now() - firstAsk.get(key)! < 120_000;
    });
    // 500 assets, four routes each (casing variants + asset_id), rendered eight at a time.
    const pages = Array.from({ length: 2_000 }, (_, i) => `SIM${i % 500}-GAAA`);
    const lane = async () => {
      for (let id = pages.shift(); id !== undefined; id = pages.shift()) {
        await Promise.all([
          m.buildFetchEnvelope(`/v1/assets/${id}`),
          m.buildFetchEnvelope(`/v1/pools?asset=${id}`, {
            softFail: true,
            attempts: 2,
          }),
        ]);
      }
    };
    const start = Date.now();
    const build = Promise.all(Array.from({ length: 8 }, lane));
    await vi.runAllTimersAsync();
    await build;
    const slept = m.buildReaskSleptMs();
    expect(slept).toBeLessThanOrEqual(m.REASK_BUDGET_MS);
    // The eight lanes' first re-asks are in flight before any comes back unhealed:
    // detail 8 × 4 s; pools 8 × 1.5 s, then 7 × 4 s before the eighth disables it.
    expect(slept).toBe(8 * 4_000 + 8 * 1_500 + 7 * 4_000);
    expect(Date.now() - start).toBeLessThan(30_000);
    expect(m.buildReaskDisabled()).toEqual(
      new Set(['/v1/assets/{id}', '/v1/pools']),
    );
    expect(m.buildBakedDegraded().size).toBe(1_000);
  });
});

describe('buildWarmUp', () => {
  afterEach(() => {
    vi.useRealTimers();
    vi.unstubAllGlobals();
  });

  it('asks each key once, memoises a complete answer and never a partial one', async () => {
    const m = await fresh();
    const calls: string[] = [];
    vi.stubGlobal(
      'fetch',
      vi.fn(async (url: string) => {
        calls.push(keyOf(url));
        if (url.includes('FAIL'))
          return { status: 500, ok: false, headers: { get: () => null } };
        const partial =
          url.includes('PART') &&
          calls.filter((c) => c === keyOf(url)).length === 1;
        return answer({ data: { k: keyOf(url) } }, partial);
      }),
    );
    await m.buildWarmUp([
      '/v1/assets/DONE-GAAA',
      '/v1/assets/PART-GAAA',
      '/v1/pools?asset=DONE',
      '/v1/assets/DONE-GAAA',
      '/v1/assets/FAIL-GAAA',
    ]);
    expect(calls).toEqual([
      '/v1/assets/DONE-GAAA',
      '/v1/assets/PART-GAAA',
      '/v1/pools?asset=DONE',
      '/v1/assets/FAIL-GAAA',
    ]);
    expect(m.buildWarmupRequests()).toBe(4);
    expect(m.buildReaskSleptMs()).toBe(0);
    expect(m.buildBakedDegraded().size).toBe(0);
    // Memoised: the page's read sends nothing.
    expect((await m.buildFetchEnvelope('/v1/assets/DONE-GAAA')).data).toEqual({
      k: '/v1/assets/DONE-GAAA',
    });
    expect(calls).toHaveLength(4);
    // Discarded: the page asks again and gets the healed answer.
    const part = await m.buildFetchEnvelope('/v1/assets/PART-GAAA');
    expect(part.degraded).toBeUndefined();
    expect(calls).toHaveLength(5);
  });

  it('keeps to MAX_IN_FLIGHT and stops starting requests at its deadline', async () => {
    vi.useFakeTimers();
    const m = await fresh();
    let current = 0;
    let peak = 0;
    vi.stubGlobal(
      'fetch',
      vi.fn(async () => {
        current++;
        peak = Math.max(peak, current);
        await new Promise((r) => setTimeout(r, 1_000));
        current--;
        return answer({ data: 1 }, false);
      }),
    );
    const paths = Array.from(
      { length: 2_000 },
      (_, i) => `/v1/assets/W${i}-GAAA`,
    );
    const done = m.buildWarmUp(paths);
    await vi.runAllTimersAsync();
    await done;
    expect(peak).toBe(m.MAX_IN_FLIGHT);
    // One second per request, MAX_IN_FLIGHT lanes, until the deadline.
    expect(m.buildWarmupRequests()).toBe(
      (m.WARMUP_DEADLINE_MS / 1_000) * m.MAX_IN_FLIGHT,
    );
  });
});
