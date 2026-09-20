import { afterEach, describe, expect, it, vi } from 'vitest';

/**
 * F100/K066: fetchVerifiedCurrencies used to wrap a single fetch in
 * try/catch and fall back to `[]` on ANY failure — a persistent API
 * outage would silently bake a catalogue with zero verified rows into
 * the static export (every asset renders as unverified) instead of
 * failing the build. It now goes through buildFetchData's fail-hard
 * contract (src/lib/buildFetch.ts), the same layer sitemap.ts and the
 * other strategy-2 pages use for their listings.
 */

afterEach(() => {
  vi.unstubAllGlobals();
  vi.resetModules();
  vi.useRealTimers();
});

describe('fetchVerifiedCurrencies', () => {
  it('fails the build instead of silently returning an empty catalogue on a persistent transport failure', async () => {
    vi.useFakeTimers();
    const fetchMock = vi.fn(async () => {
      throw new Error('simulated persistent network failure');
    });
    vi.stubGlobal('fetch', fetchMock);

    const { fetchVerifiedCurrencies } = await import('./verified-currencies');

    const resultPromise = fetchVerifiedCurrencies();
    // Attach the rejection assertion before advancing fake timers so the
    // handler exists before buildFetch's retry loop actually rejects.
    const assertion = expect(resultPromise).rejects.toThrow(
      /refusing to bake fallback HTML/,
    );
    // Let buildFetch's inter-attempt sleeps elapse so all attempts run.
    await vi.runAllTimersAsync();
    await assertion;
    expect(fetchMock.mock.calls.length).toBeGreaterThan(1);
  });
});
