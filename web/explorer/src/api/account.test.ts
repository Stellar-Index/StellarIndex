import { describe, it, expect, beforeEach, afterEach, vi } from 'vitest';

import { logout } from './account';
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
