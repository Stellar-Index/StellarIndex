import { afterEach, describe, expect, it, vi } from 'vitest';
import type { ReactNode } from 'react';
import { renderHook, waitFor } from '@testing-library/react';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';

import {
  useAsset,
  useAssets,
  useChangeSummary,
  useMarkets,
  useNativeUsdPrice,
  usePools,
} from './hooks';

function wrapper() {
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  });
  return function Wrapper({ children }: { children: ReactNode }) {
    return (
      <QueryClientProvider client={client}>{children}</QueryClientProvider>
    );
  };
}

function stubFetch(route: (url: string) => unknown) {
  vi.stubGlobal(
    'fetch',
    vi.fn().mockImplementation((input: string | URL) => {
      const body = route(String(input));
      return Promise.resolve({
        ok: body != null,
        status: body != null ? 200 : 404,
        statusText: body != null ? 'OK' : 'Not Found',
        json: async () => body ?? {},
      });
    }),
  );
}

afterEach(() => {
  vi.unstubAllGlobals();
});

// #1028: the hook coerced the canonical XLM/USD price through Number() and
// kept only `flags.stale`, dropping `frozen` — which /v1/price alone sets.
describe('useNativeUsdPrice keeps the envelope and the decimal string', () => {
  it('returns the served price string, every flag and as_of', async () => {
    stubFetch((url) =>
      url.includes('/v1/price')
        ? {
            data: { price: '0.123456789012345678' },
            as_of: '2026-09-25T10:00:00Z',
            flags: { frozen: true, frozen_checked: true, stale: false },
          }
        : null,
    );
    const { result } = renderHook(() => useNativeUsdPrice(), {
      wrapper: wrapper(),
    });
    await waitFor(() => expect(result.current.price).not.toBeNull());
    expect(result.current.price).toBe('0.123456789012345678');
    expect(result.current.flags.frozen).toBe(true);
    expect(result.current.asOf).toBe('2026-09-25T10:00:00Z');
  });
});

// #1028: useChangeSummary typed the raw envelope as the row it holds.
describe('useChangeSummary unwraps the envelope', () => {
  it('returns the row, not { data: row }', async () => {
    stubFetch(() => ({
      data: { entity_id: 'native', h24_delta_pct: 1.5 },
      as_of: '2026-09-25T10:00:00Z',
      flags: {},
    }));
    const { result } = renderHook(() => useChangeSummary('coin', 'native'), {
      wrapper: wrapper(),
    });
    await waitFor(() => expect(result.current.isSuccess).toBe(true));
    expect(result.current.data?.h24_delta_pct).toBe(1.5);
    expect(result.current.data?.entity_id).toBe('native');
  });
});

// #660: the hook layer returned only `.data`, so no client-fetched page
// could see the envelope's stale/degraded markers.
describe('list and detail hooks carry the envelope flags', () => {
  const flags = { stale: true, filters_ignored: ['q'] };
  function stubAll() {
    stubFetch(() => ({ data: [], as_of: '2026-09-25T10:00:00Z', flags }));
  }

  it('useMarkets', async () => {
    stubAll();
    const { result } = renderHook(() => useMarkets(10), { wrapper: wrapper() });
    await waitFor(() => expect(result.current.isSuccess).toBe(true));
    expect(result.current.data?.flags?.stale).toBe(true);
  });

  it('usePools', async () => {
    stubAll();
    const { result } = renderHook(() => usePools(10), { wrapper: wrapper() });
    await waitFor(() => expect(result.current.isSuccess).toBe(true));
    expect(result.current.data?.flags?.stale).toBe(true);
    expect(result.current.data?.pools).toEqual([]);
  });

  it('useAssets', async () => {
    stubAll();
    const { result } = renderHook(() => useAssets('fiat', 10, '', 'usd'), {
      wrapper: wrapper(),
    });
    await waitFor(() => expect(result.current.isSuccess).toBe(true));
    expect(result.current.data?.flags?.filters_ignored).toEqual(['q']);
  });

  it('useAsset', async () => {
    stubFetch(() => ({
      data: { asset_id: 'native' },
      flags: { unverified_ticker_collision: true },
    }));
    const { result } = renderHook(() => useAsset('native'), {
      wrapper: wrapper(),
    });
    await waitFor(() => expect(result.current.isSuccess).toBe(true));
    expect(result.current.data?.data.asset_id).toBe('native');
    expect(result.current.data?.flags?.unverified_ticker_collision).toBe(true);
  });
});
