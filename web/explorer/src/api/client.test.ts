import { describe, it, expect, vi, afterEach } from 'vitest';

import {
  apiGet,
  asExample,
  buildHeaders,
  isTimeoutError,
  timeoutSignal,
} from './client';

// [absence: timeouts] every runtime fetch needs an upper bound.
// timeoutSignal is the shared primitive that provides it while
// still honoring an external (e.g. TanStack Query) cancellation signal.
describe('timeoutSignal', () => {
  it('aborts on its own after the given timeout when no external signal is passed', async () => {
    // Real timers, deliberately: the no-external path is native
    // AbortSignal.timeout(), whose internal timer vitest fake timers do
    // NOT drive on Node 22 (CI) — the fake-timer version passed locally
    // on Node 24 and failed every CI run.
    // 30ms timeout / 150ms wait keeps it fast and 5x-margin robust.
    const signal = timeoutSignal(30);
    expect(signal.aborted).toBe(false);
    await new Promise((resolve) => setTimeout(resolve, 150));
    expect(signal.aborted).toBe(true);
  });

  it('aborts immediately when the external signal is already aborted', () => {
    const controller = new AbortController();
    controller.abort('external reason');
    const signal = timeoutSignal(60_000, controller.signal);
    expect(signal.aborted).toBe(true);
  });

  it('aborts when the external signal aborts before the timeout fires', async () => {
    vi.useFakeTimers();
    const controller = new AbortController();
    const signal = timeoutSignal(60_000, controller.signal);
    expect(signal.aborted).toBe(false);
    controller.abort('cancelled by caller');
    expect(signal.aborted).toBe(true);
    vi.useRealTimers();
  });

  it('does not abort before the timeout elapses', async () => {
    vi.useFakeTimers();
    const signal = timeoutSignal(5_000);
    await vi.advanceTimersByTimeAsync(4_000);
    expect(signal.aborted).toBe(false);
    vi.useRealTimers();
  });
});

// REGRESSION: asExample('/v1/assets/{id}', { id: slug }) rendered
// `/v1/assets/%7Bid%7D?id=<slug>` — the placeholder was never substituted
// and the param it named was dumped onto the query string instead — while
// the panel's real request hit `/v1/assets/<slug>`. The `<>` reveal must
// show the URL that was actually fetched.
describe('asExample', () => {
  it('substitutes an OpenAPI-style path placeholder from params, not the query string', () => {
    const example = asExample('/v1/assets/{id}', {
      id: 'USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN',
    });
    expect(example.url).toContain(
      '/v1/assets/USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN',
    );
    expect(example.url).not.toContain('%7Bid%7D');
    expect(example.url).not.toContain('?id=');
  });

  it('still sends leftover params as a query string alongside a substituted placeholder', () => {
    const example = asExample('/v1/accounts/{account}/graph', {
      account: 'GABC123',
      depth: 2,
    });
    expect(example.url).toContain('/v1/accounts/GABC123/graph?');
    expect(example.url).toContain('depth=2');
  });
});

// A mutation abort (timeoutSignal's own DOMException, or the
// browser's native AbortError) must read as "timed out", not fall through
// to a generic ApiError check that a bare abort never satisfies.
describe('isTimeoutError', () => {
  it('is true for the DOMException timeoutSignal raises on its own timeout', () => {
    expect(isTimeoutError(new DOMException('Request timed out', 'TimeoutError'))).toBe(
      true,
    );
  });

  it('is true for a native AbortError', () => {
    expect(isTimeoutError(new DOMException('The user aborted', 'AbortError'))).toBe(
      true,
    );
  });

  it('is false for a plain Error', () => {
    expect(isTimeoutError(new Error('offline'))).toBe(false);
  });
});

describe('apiGet', () => {
  afterEach(() => vi.restoreAllMocks());

  it('passes a bounded AbortSignal to fetch (no more unbounded requests)', async () => {
    const fetchSpy = vi
      .spyOn(globalThis, 'fetch')
      .mockResolvedValue(
        new Response(JSON.stringify({ ok: true }), { status: 200 }),
      );
    await apiGet('/v1/status');
    expect(fetchSpy).toHaveBeenCalledTimes(1);
    const init = fetchSpy.mock.calls[0]?.[1] as RequestInit | undefined;
    expect(init?.signal).toBeInstanceOf(AbortSignal);
    expect(init?.signal?.aborted).toBe(false);
  });

  const shed = (after?: string) =>
    new Response(JSON.stringify({ type: 'x/account-trades-timeout' }), {
      status: 503,
      headers: after ? { 'Retry-After': after } : {},
    });

  it('retries a shed 503 once after its Retry-After', async () => {
    vi.useFakeTimers();
    try {
      const fetchSpy = vi
        .spyOn(globalThis, 'fetch')
        .mockResolvedValueOnce(shed('5'))
        .mockResolvedValueOnce(
          new Response(JSON.stringify({ ok: true }), { status: 200 }),
        );
      const p = apiGet<{ ok: boolean }>('/v1/accounts/G/trades');
      await vi.advanceTimersByTimeAsync(4_999);
      expect(fetchSpy).toHaveBeenCalledTimes(1);
      await vi.advanceTimersByTimeAsync(1);
      await expect(p).resolves.toEqual({ ok: true });
      expect(fetchSpy).toHaveBeenCalledTimes(2);
    } finally {
      vi.useRealTimers();
    }
  });

  it('throws when the retry is shed too', async () => {
    vi.useFakeTimers();
    try {
      const fetchSpy = vi
        .spyOn(globalThis, 'fetch')
        .mockResolvedValueOnce(shed('5'))
        .mockResolvedValueOnce(shed('5'));
      const p = apiGet('/v1/accounts/G/trades');
      const done = expect(p).rejects.toThrow(/^503/);
      await vi.advanceTimersByTimeAsync(5_000);
      await done;
      expect(fetchSpy).toHaveBeenCalledTimes(2);
    } finally {
      vi.useRealTimers();
    }
  });

  it.each([['30'], [undefined]])(
    'does not retry a 503 with Retry-After %s',
    async (after) => {
      const fetchSpy = vi
        .spyOn(globalThis, 'fetch')
        .mockResolvedValueOnce(shed(after));
      await expect(apiGet('/v1/accounts/G/trades')).rejects.toThrow(/^503/);
      expect(fetchSpy).toHaveBeenCalledTimes(1);
    },
  );
});

describe('buildHeaders', () => {
  afterEach(() => {
    vi.unstubAllEnvs();
    vi.unstubAllGlobals();
  });

  it('never sends the build key from a browser', () => {
    vi.stubEnv('STELLARINDEX_BUILD_API_KEY', 'si_test_build');
    expect(buildHeaders()).toEqual({ Accept: 'application/json' });
  });

  it('sends the build key when prerendering', () => {
    vi.stubEnv('STELLARINDEX_BUILD_API_KEY', 'si_test_build');
    vi.stubGlobal('window', undefined);
    expect(buildHeaders()).toEqual({
      Accept: 'application/json',
      'X-API-Key': 'si_test_build',
    });
  });

  it('sends no key when the build has none', () => {
    vi.stubEnv('STELLARINDEX_BUILD_API_KEY', '');
    vi.stubGlobal('window', undefined);
    expect(buildHeaders()).toEqual({ Accept: 'application/json' });
  });
});
