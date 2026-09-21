import { render, screen } from '@testing-library/react';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { afterEach, describe, expect, it, vi } from 'vitest';
import type { ReactElement } from 'react';

import type { LiveTip, StreamFrame } from '@/lib/live/hooks';

import { HomeHeroChart } from './HomeHeroChart';

vi.mock('@/components/charts/MarketChart', () => ({
  MarketChart: () => null,
}));

const useTipStream = vi.hoisted(() =>
  vi.fn<
    (asset: string | null, quote?: string) => StreamFrame<LiveTip> | null
  >(),
);
vi.mock('@/lib/live/hooks', async (importOriginal) => ({
  ...(await importOriginal<typeof import('@/lib/live/hooks')>()),
  useTipStream,
  // A real clock reading so staleness verdicts run in render (the
  // production interval hasn't ticked yet inside a render test) —
  // mirrors LiveAssetPrice.test.tsx / LivePairPrice.test.tsx.
  useLiveClock: () => Date.now(),
}));

function renderHero(ui: ReactElement = <HomeHeroChart />) {
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  });
  return render(
    <QueryClientProvider client={client}>{ui}</QueryClientProvider>,
  );
}

function mockPriceFetch(price: string, stale: boolean) {
  vi.stubGlobal(
    'fetch',
    vi.fn().mockImplementation((input: string | URL) => {
      const url = String(input);
      if (url.includes('/v1/price')) {
        return Promise.resolve({
          ok: true,
          status: 200,
          json: async () => ({ data: { price }, flags: { stale } }),
        });
      }
      // /v1/chart 24h-change series — irrelevant to these assertions.
      return Promise.resolve({
        ok: false,
        status: 404,
        json: async () => ({}),
      });
    }),
  );
}

afterEach(() => {
  useTipStream.mockReset();
  vi.unstubAllGlobals();
});

describe('HomeHeroChart', () => {
  // REGRESSION (RLT-368): the "live USD price" label was hardcoded and the
  // tip stream frame was used verbatim with no staleness check, so a
  // wedged/quiet stream kept showing a stale tick captioned "live"
  // forever. Every sibling live-price widget (LivePairPrice, LiveAssetPrice)
  // gates its tip on isFrameStale; HomeHeroChart must too.
  it('a stale tip frame does not claim "live USD price" (WB-04)', async () => {
    mockPriceFetch('0.17', false);
    useTipStream.mockReturnValue({
      data: { data: { price: '0.1745' }, as_of: '2026-08-08T00:00:00Z' },
      receivedAt: Date.now() - 60_000,
    });
    renderHero();

    // The fresh polled price wins over the stale tip's stashed figure.
    expect(await screen.findByText('$0.170000')).toBeInTheDocument();
    expect(screen.queryByText('$0.174500')).not.toBeInTheDocument();
    expect(screen.queryByText(/live USD price/i)).not.toBeInTheDocument();
  });

  it('a fresh tip frame does claim "live USD price"', async () => {
    mockPriceFetch('0.17', false);
    useTipStream.mockReturnValue({
      data: { data: { price: '0.1745' }, as_of: '2026-08-08T00:00:00Z' },
      receivedAt: Date.now(),
    });
    renderHero();

    expect(await screen.findByText('$0.174500')).toBeInTheDocument();
    expect(screen.getByText(/live USD price/i)).toBeInTheDocument();
  });

  it('no tip stream and a stale polled price is captioned stale, not live', async () => {
    mockPriceFetch('0.17', true);
    useTipStream.mockReturnValue(null);
    renderHero();

    expect(await screen.findByText('$0.170000')).toBeInTheDocument();
    expect(screen.getByText(/USD price · stale/i)).toBeInTheDocument();
    expect(screen.queryByText(/live USD price/i)).not.toBeInTheDocument();
  });
});
