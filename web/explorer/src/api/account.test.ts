import { describe, it, expect, beforeEach, afterEach, vi } from 'vitest';

import { createKey, createPriceAlert, logout } from './account';
import { SESSION_HINT_COOKIE, sessionHintPresent } from './sessionHint';

beforeEach(() => {
  document.cookie = `${SESSION_HINT_COOKIE}=1; Path=/`;
});

afterEach(() => {
  vi.unstubAllGlobals();
  document.cookie = `${SESSION_HINT_COOKIE}=; Max-Age=0; Path=/`;
});

describe('logout', () => {
  it('drops the session hint so the signed-out surfaces appear at once', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn().mockResolvedValue({
        ok: true,
        status: 204,
        statusText: '204',
      } as Response),
    );

    await logout();

    expect(sessionHintPresent()).toBe(false);
  });

  it('drops the hint even when the request fails', async () => {
    // Sign-out is best-effort — both call sites bounce the visitor
    // regardless of the response — so local state must not stay
    // signed-in because the request that was meant to end the session
    // could not be delivered.
    vi.stubGlobal('fetch', vi.fn().mockRejectedValue(new Error('offline')));

    await expect(logout()).rejects.toThrow();
    expect(sessionHintPresent()).toBe(false);
  });
});

describe('idempotent creates', () => {
  const sentKeys = (fetchMock: ReturnType<typeof vi.fn>): string[] =>
    fetchMock.mock.calls.map(
      ([, init]) =>
        (init as RequestInit & { headers: Record<string, string> }).headers[
          'Idempotency-Key'
        ],
    );
  const created = (): Response =>
    ({
      ok: true,
      status: 201,
      statusText: '201',
      json: async () => ({}),
    }) as Response;

  it('retries a timed-out create with the same Idempotency-Key, then rotates it', async () => {
    // First attempt dies client-side (timeout) — it may still have
    // committed, so the retry must be recognisable as the same mint.
    const fetchMock = vi
      .fn()
      .mockRejectedValueOnce(new DOMException('timed out', 'TimeoutError'))
      .mockResolvedValue(created());
    vi.stubGlobal('fetch', fetchMock);

    await expect(createKey({ name: 'prod' })).rejects.toThrow();
    await createKey({ name: 'prod' });
    await createKey({ name: 'prod' });

    const [timedOut, retry, fresh] = sentKeys(fetchMock);
    expect(timedOut).toMatch(/^[0-9a-f-]{32,36}$/);
    expect(retry).toBe(timedOut);
    // After a success the next submission is a new resource.
    expect(fresh).not.toBe(timedOut);
  });

  it('gives a changed request body a fresh key', async () => {
    const fetchMock = vi
      .fn()
      .mockRejectedValueOnce(new Error('offline'))
      .mockResolvedValue(created());
    vi.stubGlobal('fetch', fetchMock);

    const alert: Parameters<typeof createPriceAlert>[0] = {
      base_asset: 'native',
      quote_asset: 'fiat:USD',
      condition: 'above',
      threshold: '1',
    };
    await expect(createPriceAlert(alert)).rejects.toThrow();
    await createPriceAlert({ ...alert, threshold: '2' });

    const [first, second] = sentKeys(fetchMock);
    expect(first).toBeTruthy();
    expect(second).toBeTruthy();
    expect(second).not.toBe(first);
  });
});
