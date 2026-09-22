import { renderHook } from '@testing-library/react';
import { act } from '@testing-library/react';
import { describe, expect, it, vi } from 'vitest';

// Drive the REAL path (subscribeStream → useStreamJSON → useTipStream)
// rather than stubbing the hook under test, same approach as
// ledger-follow.test.tsx.
const listeners: Array<(data: string) => void> = [];
vi.mock('./streams', () => ({
  subscribeStream: (
    _url: string,
    _type: string,
    onData: (d: string) => void,
  ) => {
    listeners.push(onData);
    return () => {};
  },
}));

const { useTipStream } = await import('./hooks');

// RLT-370: the server's tip_update frame (internal/api/v1/price_tip_stream.go
// tipStreamPayload) carries `data.observed_at` and a `flags` block
// (divergence_warning, frozen, ...), but the LiveTip type declared here only
// named {price, price_type, window_seconds}/{as_of, sources} — so a consumer
// could not read the divergence/frozen verdict on a live tick without an
// unsafe cast, and nothing in the tree did.
function pushTip(payload: unknown) {
  act(() => {
    for (const l of [...listeners]) {
      l(JSON.stringify(payload));
    }
  });
}

describe('useTipStream', () => {
  it('exposes observed_at and divergence/frozen flags off a tip_update frame', () => {
    const { result } = renderHook(() => useTipStream('native'));

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

    expect(result.current?.data.data.observed_at).toBe(
      '2026-09-22T00:00:00Z',
    );
    expect(result.current?.data.flags?.divergence_warning).toBe(true);
    expect(result.current?.data.flags?.frozen).toBe(false);
  });
});
