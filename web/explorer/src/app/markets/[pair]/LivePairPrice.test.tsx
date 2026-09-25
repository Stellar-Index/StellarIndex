import { render, screen } from '@testing-library/react';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import type { ReactElement } from 'react';

import type { LiveTip, StreamFrame } from '@/lib/live/hooks';

import { LivePairPrice } from './LivePairPrice';

const useTipStream = vi.hoisted(() =>
  vi.fn<(base: string, quote?: string) => StreamFrame<LiveTip> | null>(),
);
vi.mock('@/lib/live/hooks', async (importOriginal) => ({
  ...(await importOriginal<typeof import('@/lib/live/hooks')>()),
  useTipStream,
  // A real clock reading so staleness verdicts run in render (the
  // production interval hasn't ticked yet inside a render test).
  useLiveClock: () => Date.now(),
}));

// LivePairPrice now also runs useChangeSummary (a TanStack Query
// consumer, K061) alongside its hand-rolled price poll — every render
// needs a QueryClient.
function renderPrice(ui: ReactElement) {
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  });
  return render(
    <QueryClientProvider client={client}>{ui}</QueryClientProvider>,
  );
}

beforeEach(() => {
  vi.stubGlobal('fetch', vi.fn().mockRejectedValue(new Error('offline')));
});
afterEach(() => {
  useTipStream.mockReset();
  vi.unstubAllGlobals();
});

// REGRESSION (K061): markets/[pair]/page.tsx computed its 24h change
// badge ONCE at build time from the chart's first/last points and
// handed it to LivePairPrice as a static prop — the price beside it
// kept refreshing live (poll + tip stream), so a large intraday move
// could leave the badge pointing the wrong direction for as long as
// the tab stayed open. LivePairPrice must re-derive the badge from the
// live change-summary worker (GET /v1/changes/pair/{base}/{quote}),
// overriding the baked figure once the worker reports a fresher one —
// mirrors the asset-sidebar fix (F090).
describe('LivePairPrice — 24h change badge (K061)', () => {
  function mockChangesFetch(h24DeltaPct: number) {
    vi.stubGlobal(
      'fetch',
      vi.fn().mockImplementation((input: string | URL) => {
        const url = String(input);
        if (url.includes('/v1/changes/')) {
          // The pair id ("crypto:XLM/fiat:USD") must occupy ONE percent
          // -encoded path segment — an unencoded "/" splits the request
          // across two segments and matches no route.
          expect(url).toContain('/v1/changes/pair/crypto%3AXLM%2Ffiat%3AUSD');
          expect(url).not.toContain('/v1/changes/pair/crypto:XLM/fiat:USD');
          return Promise.resolve({
            ok: true,
            status: 200,
            json: async () => ({
              data: {
                entity_type: 'pair',
                entity_id: 'crypto:XLM/fiat:USD',
                refreshed_at: '2026-09-19T15:00:00Z',
                current_value: '0.170',
                h24_delta_pct: h24DeltaPct,
              },
            }),
          });
        }
        // /v1/price poll — irrelevant here, keep the baked price.
        return Promise.resolve({
          ok: false,
          status: 404,
          json: async () => ({}),
        });
      }),
    );
  }

  it('overrides a stale build-time UP badge with a live DOWN figure from the change-summary worker', async () => {
    useTipStream.mockReturnValue(null);
    mockChangesFetch(-5.2);
    renderPrice(
      <LivePairPrice
        base="crypto:XLM"
        quote="fiat:USD"
        initialPrice={0.17}
        initialObservedAt={null}
        quoteIsUsd
        quoteSuffix="USD"
        initialChangePct={2.1}
      />,
    );

    // The live worker says -5.20% (DOWN); the build-time bake said
    // +2.10% (UP). The rendered badge must reflect the LIVE figure, not
    // the stale baked one that started it pointing the wrong direction.
    expect(await screen.findByText(/-5\.20%/)).toBeInTheDocument();
    expect(screen.queryByText(/\+2\.10%/)).not.toBeInTheDocument();
  });

  it('keeps the baked badge when the change-summary worker has no row yet', () => {
    useTipStream.mockReturnValue(null);
    // Default beforeEach fetch stub rejects every request — no worker row.
    renderPrice(
      <LivePairPrice
        base="crypto:XLM"
        quote="fiat:USD"
        initialPrice={0.17}
        initialObservedAt={null}
        quoteIsUsd
        quoteSuffix="USD"
        initialChangePct={2.1}
      />,
    );
    expect(screen.getByText(/\+2\.10%/)).toBeInTheDocument();
  });
});

// REGRESSION (GH-772): the withheld caption was a hardcoded liquidity
// -only string regardless of WHY the server refused to serve a price,
// and a live tip's own divergence/frozen flags never reached this
// surface at all.
describe('LivePairPrice — withheld wording + tip flags (GH-772)', () => {
  it('sources the withheld caption from the server problem body, not a hardcoded liquidity string', async () => {
    useTipStream.mockReturnValue(null);
    vi.stubGlobal(
      'fetch',
      vi.fn().mockResolvedValue({
        ok: false,
        status: 404,
        json: async () => ({
          type: 'https://api.stellarindex.io/errors/price-withheld',
          title: 'Price withheld — issuer flagged',
          detail:
            'a directory-flagged issuer is on one leg of crypto:XLM / fiat:USD, so no price is published',
        }),
      }),
    );
    renderPrice(
      <LivePairPrice
        base="crypto:XLM"
        quote="fiat:USD"
        initialPrice={0.17}
        initialObservedAt={null}
        quoteIsUsd
        quoteSuffix="USD"
      />,
    );
    expect(
      await screen.findByText(/directory-flagged issuer/i),
    ).toBeInTheDocument();
    expect(
      screen.queryByText(/market too thin to aggregate/i),
    ).not.toBeInTheDocument();
  });

  it('marks a live tip tick as frozen when the stream flags it', () => {
    useTipStream.mockReturnValue({
      data: {
        data: { price: '0.1745' },
        as_of: '2026-09-25T00:00:00Z',
        flags: { frozen: true, frozen_checked: true },
      },
      receivedAt: Date.now(),
    });
    renderPrice(
      <LivePairPrice
        base="crypto:XLM"
        quote="fiat:USD"
        initialPrice={0.17}
        initialObservedAt={null}
        quoteIsUsd
        quoteSuffix="USD"
      />,
    );
    expect(screen.getByText(/frozen/i)).toBeInTheDocument();
  });
});
