import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { renderHook } from '@testing-library/react';
import { act } from '@testing-library/react';
import type { ReactNode } from 'react';
import { afterEach, describe, expect, it, vi } from 'vitest';

// Drive the REAL path (subscribeStream → useStreamJSON → useTipStream)
// rather than stubbing the hook under test, same approach as
// ledger-follow.test.tsx.
const listeners: Array<{ type: string; onData: (data: string) => void }> = [];
vi.mock('./streams', () => ({
  subscribeStream: (
    _url: string,
    type: string,
    onData: (d: string) => void,
  ) => {
    listeners.push({ type, onData });
    return () => {};
  },
}));

const { useTipStream } = await import('./hooks');

function push(type: string, payload: unknown) {
  act(() => {
    for (const l of [...listeners]) {
      if (l.type === type) l.onData(JSON.stringify(payload));
    }
  });
}

// The server's tip_update frame (internal/api/v1/price_tip_stream.go
// tipStreamPayload) carries `data.observed_at` and a `flags` block
// (divergence_warning, frozen, ...), but the LiveTip type declared here only
// named {price, price_type, window_seconds}/{as_of, sources} — so a consumer
// could not read the divergence/frozen verdict on a live tick without an
// unsafe cast, and nothing in the tree did.
function pushTip(payload: unknown) {
  push('tip_update', payload);
}

function withClient(client = new QueryClient()) {
  return function Wrapper({ children }: { children: ReactNode }) {
    return (
      <QueryClientProvider client={client}>{children}</QueryClientProvider>
    );
  };
}

const TIP = {
  data: { price: '0.115', price_type: 'vwap', window_seconds: 60 },
  as_of: '2026-09-22T00:00:05Z',
  sources: ['sdex'],
};

// tipWithheldPayload in internal/api/v1/price_tip_stream.go.
const WITHHELD = {
  asset_id: 'native',
  quote: 'fiat:USD',
  reason: 'scam_issuer',
  as_of: '2026-09-22T00:00:10Z',
};

afterEach(() => {
  listeners.length = 0;
  vi.restoreAllMocks();
});

describe('useTipStream', () => {
  it('exposes observed_at and divergence/frozen flags off a tip_update frame', () => {
    const { result } = renderHook(() => useTipStream('native'), {
      wrapper: withClient(),
    });

    pushTip({
      data: {
        price: '0.115',
        price_type: 'vwap',
        window_seconds: 60,
        observed_at: '2026-09-22T00:00:00Z',
      },
      as_of: '2026-09-22T00:00:05Z',
      sources: ['sdex'],
      flags: { divergence_warning: true, frozen: false },
    });

    expect(result.current?.data.data.observed_at).toBe('2026-09-22T00:00:00Z');
    expect(result.current?.data.flags?.divergence_warning).toBe(true);
    expect(result.current?.data.flags?.frozen).toBe(false);
  });

  it('drops the live tip once the server withholds the pair, until ticks resume', () => {
    const client = new QueryClient();
    const invalidate = vi.spyOn(client, 'invalidateQueries');
    const now = vi.spyOn(Date, 'now').mockReturnValue(1_000);
    const { result } = renderHook(() => useTipStream('native'), {
      wrapper: withClient(client),
    });

    pushTip(TIP);
    expect(result.current?.data.data.price).toBe('0.115');

    now.mockReturnValue(6_000);
    push('price_withheld', WITHHELD);
    expect(result.current).toBeNull();
    expect(invalidate).toHaveBeenCalledWith({
      queryKey: ['/v1/price', 'native', 'fiat:USD'],
    });

    now.mockReturnValue(11_000);
    pushTip({ ...TIP, data: { ...TIP.data, price: '0.12' } });
    expect(result.current?.data.data.price).toBe('0.12');
  });
});
