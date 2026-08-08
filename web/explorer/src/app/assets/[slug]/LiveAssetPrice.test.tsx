import { render, screen } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import type { LiveTip, StreamFrame } from '@/lib/live/hooks';

import { LiveAssetPrice } from './LiveAssetPrice';

const useTipStream = vi.hoisted(() =>
  vi.fn<(asset: string | null, quote?: string) => StreamFrame<LiveTip> | null>(),
);
vi.mock('@/lib/live/hooks', async (importOriginal) => ({
  ...(await importOriginal<typeof import('@/lib/live/hooks')>()),
  useTipStream,
  // A real clock reading so staleness verdicts run in render (the
  // production interval hasn't ticked yet inside a render test).
  useLiveClock: () => Date.now(),
}));

beforeEach(() => {
  // The 60s /v1/price poll fallback: fail it so tests exercise pure
  // baked-value + stream behavior deterministically.
  vi.stubGlobal('fetch', vi.fn().mockRejectedValue(new Error('offline')));
});
afterEach(() => {
  useTipStream.mockReset();
  vi.unstubAllGlobals();
});

describe('LiveAssetPrice', () => {
  it('falls back to the baked price + provenance when no stream frames arrive', () => {
    useTipStream.mockReturnValue(null);
    render(
      <LiveAssetPrice assetID="native" initialPrice={0.17} initialProvenance="vwap1m" />,
    );
    expect(screen.getByText(/\$0\.17/)).toBeInTheDocument();
    expect(screen.getByText(/1-min VWAP · USD/i)).toBeInTheDocument();
    expect(screen.queryByRole('status', { name: 'live' })).not.toBeInTheDocument();
  });

  it('a fresh tip frame takes over the headline with the live caption', () => {
    useTipStream.mockReturnValue({
      data: {
        data: { price: '0.1745' },
        as_of: '2026-08-08T00:00:00Z',
        sources: ['sdex'],
      },
      receivedAt: Date.now(),
    });
    render(
      <LiveAssetPrice assetID="native" initialPrice={0.17} initialProvenance="vwap1m" />,
    );
    expect(screen.getByText(/\$0\.1745/)).toBeInTheDocument();
    expect(screen.getByText(/live tip price · USD · streaming/i)).toBeInTheDocument();
    expect(screen.getByRole('status', { name: 'live' })).toBeInTheDocument();
  });

  it('a stale tip frame does NOT claim live (WB-04)', () => {
    useTipStream.mockReturnValue({
      data: { data: { price: '0.1745' }, as_of: '2026-08-08T00:00:00Z' },
      receivedAt: Date.now() - 60_000,
    });
    render(
      <LiveAssetPrice assetID="native" initialPrice={0.17} initialProvenance="vwap1m" />,
    );
    expect(screen.getByText(/\$0\.17/)).toBeInTheDocument();
    expect(screen.queryByText(/streaming/i)).not.toBeInTheDocument();
  });
});
