import { renderHook, waitFor } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';

import { usePricePoll } from './hooks';

// REGRESSION (T304): usePricePoll's tick() called raw fetch() with no
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

    const { unmount } = renderHook(() => usePricePoll({ asset: 'native' }));

    await waitFor(() => expect(capturedSignal).toBeInstanceOf(AbortSignal));
    expect(capturedSignal?.aborted).toBe(false);

    unmount();

    expect(capturedSignal?.aborted).toBe(true);
  });
});
