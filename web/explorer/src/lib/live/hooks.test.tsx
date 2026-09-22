import { renderHook, waitFor } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';

import { usePricePoll } from './hooks';

// T275: the /v1/price poll fetch had no signal, so a hung connection left
// the poll (and anything reading `polled`) waiting forever with no way to
// recover before the next interval. Every fetch call must now carry a
// bounded AbortSignal.
describe('usePricePoll', () => {
  afterEach(() => {
    vi.unstubAllGlobals();
  });

  it('bounds the /v1/price fetch with an AbortSignal', async () => {
    const fetchMock = vi.fn(
      async (_input: RequestInfo | URL, _init?: RequestInit) =>
        new Response(
          JSON.stringify({
            data: { price: '0.5', observed_at: '2026-01-01T00:00:00Z' },
            flags: {},
          }),
          { status: 200, headers: { 'content-type': 'application/json' } },
        ),
    );
    vi.stubGlobal('fetch', fetchMock);

    const { result } = renderHook(() =>
      usePricePoll({ asset: 'native', quote: 'fiat:USD' }),
    );

    await waitFor(() => expect(result.current.polled).toBe(true));

    expect(fetchMock).toHaveBeenCalledTimes(1);
    const [, init] = fetchMock.mock.calls[0];
    expect(init?.signal).toBeInstanceOf(AbortSignal);
  });

  // T312: a background tab kept polling /v1/price on the full interval,
  // burning API quota and battery for data nobody was looking at.
  it('skips the poll tick while the tab is hidden', async () => {
    vi.useFakeTimers();
    const fetchMock = vi.fn(
      async () =>
        new Response(
          JSON.stringify({
            data: { price: '0.5', observed_at: '2026-01-01T00:00:00Z' },
            flags: {},
          }),
          { status: 200, headers: { 'content-type': 'application/json' } },
        ),
    );
    vi.stubGlobal('fetch', fetchMock);

    const hiddenSpy = vi
      .spyOn(document, 'hidden', 'get')
      .mockReturnValue(true);

    renderHook(() =>
      usePricePoll({ asset: 'native', quote: 'fiat:USD', intervalMs: 1000 }),
    );

    // Initial synchronous tick is skipped because the tab is hidden.
    expect(fetchMock).not.toHaveBeenCalled();

    await vi.advanceTimersByTimeAsync(5000);

    expect(fetchMock).not.toHaveBeenCalled();

    hiddenSpy.mockRestore();
    vi.useRealTimers();
  });
});
