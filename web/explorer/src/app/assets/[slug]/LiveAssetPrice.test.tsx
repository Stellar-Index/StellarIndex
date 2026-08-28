import { render, screen } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import type { LiveTip, StreamFrame } from '@/lib/live/hooks';

import { LiveAssetPrice } from './LiveAssetPrice';

const useTipStream = vi.hoisted(() =>
  vi.fn<
    (asset: string | null, quote?: string) => StreamFrame<LiveTip> | null
  >(),
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
      <LiveAssetPrice
        assetID="native"
        initialPrice={0.17}
        initialProvenance="vwap1m"
      />,
    );
    expect(screen.getByText(/\$0\.17/)).toBeInTheDocument();
    expect(screen.getByText(/1-min VWAP · USD/i)).toBeInTheDocument();
    expect(
      screen.queryByRole('status', { name: 'live' }),
    ).not.toBeInTheDocument();
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
      <LiveAssetPrice
        assetID="native"
        initialPrice={0.17}
        initialProvenance="vwap1m"
      />,
    );
    expect(screen.getByText(/\$0\.1745/)).toBeInTheDocument();
    expect(
      screen.getByText(/live tip price · USD · streaming/i),
    ).toBeInTheDocument();
    expect(screen.getByRole('status', { name: 'live' })).toBeInTheDocument();
  });

  it('a baked declared-peg price renders the pegged caption, never a market claim', () => {
    useTipStream.mockReturnValue(null);
    render(
      <LiveAssetPrice
        assetID="AUDD-GDC7X2MXTYSAKUUGAIQ7J7RPEIM7GXSAIWFYWWH4GLNFECQVJJLB2EEU"
        initialPrice={0.655}
        initialProvenance="declared_peg"
      />,
    );
    expect(screen.getByText(/\$0\.655/)).toBeInTheDocument();
    expect(
      screen.getByText(
        /pegged · declared 1:1 fiat peg × fx rate · not a market price/i,
      ),
    ).toBeInTheDocument();
    expect(screen.queryByText(/VWAP/i)).not.toBeInTheDocument();
  });

  it('a price-withheld poll verdict does NOT blank a declared-peg price', async () => {
    // The withheld-replaces-baked rule exists to purge lower-trust
    // MARKET snapshots; a declared-peg basis is not a market claim, so
    // the peg price + caption must survive the server's (expected)
    // price-withheld verdict for the same asset's market books.
    useTipStream.mockReturnValue(null);
    vi.stubGlobal(
      'fetch',
      vi.fn().mockResolvedValue({
        status: 404,
        ok: false,
        json: async () => ({
          type: 'https://api.stellarindex.io/errors/price-withheld',
        }),
      }),
    );
    render(
      <LiveAssetPrice
        assetID="AUDD-GDC7X2MXTYSAKUUGAIQ7J7RPEIM7GXSAIWFYWWH4GLNFECQVJJLB2EEU"
        initialPrice={0.655}
        initialProvenance="declared_peg"
      />,
    );
    // Wait for the poll's withheld verdict to land, then assert the
    // peg price is still on screen with its honest caption.
    await vi.waitFor(() => {
      expect(fetch).toHaveBeenCalled();
    });
    expect(await screen.findByText(/\$0\.655/)).toBeInTheDocument();
    expect(
      screen.getByText(
        /pegged · declared 1:1 fiat peg × fx rate · not a market price/i,
      ),
    ).toBeInTheDocument();
    expect(screen.queryByText(/price withheld/i)).not.toBeInTheDocument();
  });

  it('a price-withheld poll verdict still replaces a market-provenance baked price', async () => {
    useTipStream.mockReturnValue(null);
    vi.stubGlobal(
      'fetch',
      vi.fn().mockResolvedValue({
        status: 404,
        ok: false,
        json: async () => ({
          type: 'https://api.stellarindex.io/errors/price-withheld',
        }),
      }),
    );
    render(
      <LiveAssetPrice
        assetID="native"
        initialPrice={0.17}
        initialProvenance="listing"
      />,
    );
    expect(
      await screen.findByText(/price withheld · market too thin to aggregate/i),
    ).toBeInTheDocument();
    expect(screen.queryByText(/\$0\.17/)).not.toBeInTheDocument();
  });

  it('a stale tip frame does NOT claim live (WB-04)', () => {
    useTipStream.mockReturnValue({
      data: { data: { price: '0.1745' }, as_of: '2026-08-08T00:00:00Z' },
      receivedAt: Date.now() - 60_000,
    });
    render(
      <LiveAssetPrice
        assetID="native"
        initialPrice={0.17}
        initialProvenance="vwap1m"
      />,
    );
    expect(screen.getByText(/\$0\.17/)).toBeInTheDocument();
    expect(screen.queryByText(/streaming/i)).not.toBeInTheDocument();
  });
});

// REGRESSION (2026-08-28): the transitive price was invisible in the UI.
//
// /v1/price answers for DIRECT markets only, so a two-hop asset gets
// price:null there while /v1/assets serves a real substance-gated
// figure (measured on CAUP7: null vs 7768.93, basis "transitive").
// AssetPathView passed initialPrice={null} regardless, so exactly the
// assets transitive pricing was built for rendered a permanent "—".
describe('LiveAssetPrice — transitive provenance', () => {
  it('renders a transitive price with an honest caption', () => {
    useTipStream.mockReturnValue(null);
    render(
      <LiveAssetPrice
        assetID="CAUP7NFABXE5TJRL3FKTPMWRLC7IAXYDCTHQRFSCLR5TMGKHOOQO772J"
        initialPrice={7768.93}
        initialProvenance="transitive"
      />,
    );
    expect(screen.getByText(/7,?768/)).toBeInTheDocument();
    expect(screen.getByText(/two-hop DEX route/i)).toBeInTheDocument();
  });

  // A transitive price is fetched live from /v1/assets on this render —
  // it is the POLL that has nothing to say, not the price that is old.
  it('does not caption a transitive price "as baked at deploy"', () => {
    useTipStream.mockReturnValue(null);
    render(
      <LiveAssetPrice
        assetID="CAUP7NFABXE5TJRL3FKTPMWRLC7IAXYDCTHQRFSCLR5TMGKHOOQO772J"
        initialPrice={7768.93}
        initialProvenance="transitive"
      />,
    );
    expect(screen.queryByText(/as baked at deploy/i)).not.toBeInTheDocument();
  });

  // It must NOT be captioned as the aggregator's FX cross-rate: that is
  // a different derivation with a different trust story.
  it('is not labelled "triangulated via XLM"', () => {
    useTipStream.mockReturnValue(null);
    render(
      <LiveAssetPrice
        assetID="CAUP7NFABXE5TJRL3FKTPMWRLC7IAXYDCTHQRFSCLR5TMGKHOOQO772J"
        initialPrice={7768.93}
        initialProvenance="transitive"
      />,
    );
    expect(screen.queryByText(/triangulated via XLM/i)).not.toBeInTheDocument();
  });
});
