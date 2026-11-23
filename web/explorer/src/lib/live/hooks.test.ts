import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { renderHook, waitFor } from '@testing-library/react';
import { createElement } from 'react';
import { afterEach, describe, expect, it, vi } from 'vitest';

import { priceIntegrityCaveats, usePricePoll } from './hooks';

describe('priceIntegrityCaveats', () => {
  it('names an unchecked peg, so its absent depeg flag never reads as clear', () => {
    expect(priceIntegrityCaveats({ proxy_deviation_unchecked: true })).toEqual([
      'peg not verified at this time',
    ]);
  });

  it('keeps the depeg warning beside an unchecked peg', () => {
    expect(
      priceIntegrityCaveats({
        proxy_deviation: true,
        proxy_deviation_unchecked: true,
      }),
    ).toEqual([
      'assumes a USD peg that is more than 2% off $1',
      'peg not verified at this time',
    ]);
  });
});

// REGRESSION: usePricePoll's tick() called raw fetch() with no
// AbortController/signal — the `cancelled` flag only gated the subsequent
// setState calls, never the in-flight request itself, so a hung /v1/price
// round trip outlived the component that started it. Every other request
// in the app goes through client.ts's timeoutSignal(); this hook was the
// one exemption.
describe('usePricePoll', () => {
  afterEach(() => {
    vi.restoreAllMocks();
    vi.unstubAllGlobals();
  });

  it('aborts the in-flight /v1/price request when the hook unmounts', async () => {
    let capturedSignal: AbortSignal | undefined;
    vi.stubGlobal(
      'fetch',
      vi.fn().mockImplementation((_url: string, init?: RequestInit) => {
        capturedSignal = init?.signal ?? undefined;
        // Simulate a hang: never resolves on its own.
        return new Promise<Response>(() => {});
      }),
    );

    const client = new QueryClient();
    const { unmount } = renderHook(() => usePricePoll({ asset: 'native' }), {
      wrapper: ({ children }) =>
        createElement(QueryClientProvider, { client }, children),
    });

    await waitFor(() => expect(capturedSignal).toBeInstanceOf(AbortSignal));
    expect(capturedSignal?.aborted).toBe(false);

    unmount();

    expect(capturedSignal?.aborted).toBe(true);
  });
});
