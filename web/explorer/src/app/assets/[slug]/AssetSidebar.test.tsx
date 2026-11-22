import { afterEach, describe, it, expect, vi } from 'vitest';
import { render, waitFor } from '@testing-library/react';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import type { ReactElement } from 'react';

vi.mock('@/api/client', async () => {
  const actual =
    await vi.importActual<typeof import('@/api/client')>('@/api/client');
  return {
    ...actual,
    apiGet: vi.fn().mockRejectedValue(new Error('offline')),
  };
});

import { apiGet } from '@/api/client';

import {
  AssetIdentityCard,
  AssetSidebar,
  type SidebarCoin,
} from './AssetSidebar';

// AssetIdentityCard embeds ChangeSummaryStrip (a TanStack Query consumer),
// so renders need a QueryClient. Retries off — a test must not poll.
function renderWithQuery(ui: ReactElement) {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(<QueryClientProvider client={qc}>{ui}</QueryClientProvider>);
}

// The headline price must SAY what it is: a bare dollar figure cannot
// show whether it came from the live
// closed-1m VWAP or from a build-time listing snapshot up to 7 days
// old — indistinguishable on screen. These tests pin the provenance
// caption semantics (text, not classes).

const coin: SidebarCoin = {
  code: 'USDC',
  slug: 'USDC',
  asset_id: 'USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN',
  observation_count: 10,
  price_usd: '0.9998',
};

describe('AssetIdentityCard price provenance caption', () => {
  it('labels a live closed-bucket price as 1-min VWAP', () => {
    const { container } = renderWithQuery(
      <AssetIdentityCard
        coin={coin}
        priceUSD="0.9998"
        priceProvenance="vwap1m"
      />,
    );
    expect(container.textContent).toContain('1-min VWAP · USD');
  });

  it('labels a triangulated price as such', () => {
    const { container } = renderWithQuery(
      <AssetIdentityCard
        coin={coin}
        priceUSD="0.9998"
        priceProvenance="triangulated"
      />,
    );
    expect(container.textContent).toContain('triangulated via XLM');
  });

  it('labels the build-time listing fallback as not-live', () => {
    const { container } = renderWithQuery(
      <AssetIdentityCard
        coin={coin}
        priceUSD="0.9998"
        priceProvenance="listing"
      />,
    );
    expect(container.textContent).toContain('listing snapshot');
    expect(container.textContent).toContain('not a live aggregated price');
  });

  it('marks a stale price', () => {
    const { container } = renderWithQuery(
      <AssetIdentityCard
        coin={coin}
        priceUSD="0.9998"
        priceProvenance="vwap1m"
        priceStale
      />,
    );
    expect(container.textContent).toContain('stale');
  });

  it('renders no caption when there is no price', () => {
    const { container } = renderWithQuery(
      <AssetIdentityCard coin={coin} priceUSD={null} priceProvenance={null} />,
    );
    expect(container.textContent).not.toContain('VWAP');
    expect(container.textContent).not.toContain('listing snapshot');
  });
});

// The detail carries the floor flags beside the figures; the rail printed
// a trustline-sum cap and a partly valued volume as exact.
describe('AssetSidebar floor marks', () => {
  const figures = {
    market_cap_usd: '5000000',
    volume_24h_usd: '1200',
    circulating_supply: '50000000000000',
    decimals: 7,
  };

  it('prefixes ≥ on a floor cap, volume and supply', () => {
    const { getByText, getAllByText } = renderWithQuery(
      <AssetSidebar
        coin={coin}
        detail={{
          ...figures,
          volume_lower_bound: true,
          supply_basis: 'classic_trustline_sum',
        }}
        priceUSD="0.9998"
      />,
    );
    expect(getByText('≥ $5M')).toBeTruthy();
    expect(getByText('≥ $1.2K')).toBeTruthy();
    expect(getByText('≥ 5M USDC')).toBeTruthy();
    // Both sides are floors, so the ratio is bounded on neither.
    expect(getAllByText('—').length).toBeGreaterThan(0);
  });

  it('marks total supply and honours the server market-cap flag', () => {
    const { getAllByText, getByText } = renderWithQuery(
      <AssetSidebar
        coin={coin}
        detail={{
          ...figures,
          total_supply: '60000000000000',
          market_cap_lower_bound: true,
          supply_basis: 'issuer_exclusion',
        }}
        priceUSD="0.9998"
      />,
    );
    expect(getByText('≥ $5M')).toBeTruthy();
    expect(getAllByText('≥ 6M USDC').length).toBe(1);
  });

  it('marks a derived lake-flows supply as an upper reading', () => {
    const { getByText } = renderWithQuery(
      <AssetSidebar
        coin={coin}
        detail={{ ...figures, supply_basis: 'classic_lake_flows' }}
        priceUSD="0.9998"
      />,
    );
    expect(getByText('≤ $5M')).toBeTruthy();
    expect(getByText('≤ 5M USDC')).toBeTruthy();
  });

  // The real shape: the detail has no supply fields, the listing row carries
  // the lake-flows supply and its basis. The mark must follow the figure.
  it('marks a lake-flows supply taken from the listing row when the detail has none', () => {
    const { getByText } = renderWithQuery(
      <AssetSidebar
        coin={{
          ...coin,
          circulating_supply: '50000000000000',
          decimals: 7,
          supply_basis: 'classic_lake_flows',
        }}
        detail={{ decimals: 7 }}
        priceUSD="0.9998"
      />,
    );
    expect(getByText('≤ 5M USDC')).toBeTruthy();
  });

  it('leaves exact figures unmarked', () => {
    const { getByText, queryByText } = renderWithQuery(
      <AssetSidebar
        coin={coin}
        detail={{ ...figures, supply_basis: 'issuer_exclusion' }}
        priceUSD="0.9998"
      />,
    );
    expect(getByText('$5M')).toBeTruthy();
    expect(getByText('0.02%')).toBeTruthy();
    expect(queryByText(/≥/)).toBeNull();
  });
});

// The 24h series is baked at deploy: the panel must say so, and must go
// with the headline when the server withholds the price.
describe('AssetSidebar price range', () => {
  const ranged: SidebarCoin = {
    ...coin,
    price_history_24h: [
      { t: '2026-01-01T00:00:00Z', p: '0.19' },
      { t: '2026-01-01T12:00:00Z', p: '0.21' },
    ],
  };
  function stubPrice(status: number, body: unknown) {
    vi.stubGlobal(
      'fetch',
      vi.fn().mockImplementation(async (input: string | URL) => {
        const code = String(input).includes('/v1/price?') ? status : 500;
        return { status: code, ok: code === 200, json: async () => body };
      }),
    );
  }
  afterEach(() => vi.unstubAllGlobals());

  it('captions the range as baked at deploy', () => {
    stubPrice(500, {});
    const { getByText } = renderWithQuery(
      <AssetSidebar coin={ranged} detail={null} priceUSD="0.2" />,
    );
    expect(getByText(/24h · USD · as baked at deploy/)).toBeTruthy();
  });

  it('withdraws the range when the server withholds the price', async () => {
    stubPrice(404, {
      type: 'https://stellarindex.io/problems/price-withheld',
      title: 'withheld',
    });
    const { queryByText } = renderWithQuery(
      <AssetSidebar coin={ranged} detail={null} priceUSD="0.2" />,
    );
    await waitFor(() => expect(queryByText('Price range')).toBeNull());
  });
});

// The market and activity figures are baked into the static export; the
// live /v1/assets/{id} row replaces them, and the baked ones say so.
describe('AssetSidebar live market figures', () => {
  const baked = {
    market_cap_usd: '5000000',
    volume_24h_usd: '1200',
    fdv_usd: '8000000',
    decimals: 7,
  };
  const bakedCoin: SidebarCoin = { ...coin, trade_count_24h: 1500 };
  function liveRow(row: Record<string, unknown> | null) {
    vi.mocked(apiGet).mockImplementation(async (path: string) => {
      if (row && path === `/v1/assets/${encodeURIComponent(coin.asset_id)}`)
        return { data: row };
      throw new Error('offline');
    });
  }
  afterEach(() => vi.mocked(apiGet).mockReset());

  it('replaces the baked figures with the live row', async () => {
    liveRow({
      ...coin,
      market_cap_usd: '7000000',
      volume_24h_usd: '3000',
      fdv_usd: '9000000',
      trade_count_24h: 4321,
    });
    const { findByText, getByText, queryByText } = renderWithQuery(
      <AssetSidebar coin={bakedCoin} detail={baked} priceUSD="0.9998" />,
    );
    expect(await findByText('$7M')).toBeTruthy();
    expect(getByText('$3K')).toBeTruthy();
    expect(getByText('$9M')).toBeTruthy();
    expect(getByText('4.32K')).toBeTruthy();
    expect(queryByText('$5M')).toBeNull();
    expect(queryByText('as baked at deploy')).toBeNull();
  });

  it('hides a USD figure the live row withholds instead of falling back', async () => {
    liveRow({
      ...coin,
      price_usd: null,
      price_withheld_reason: 'substance',
      market_cap_usd: null,
      fdv_usd: null,
      volume_24h_usd: '3000',
    });
    const { findByText, queryByText } = renderWithQuery(
      <AssetSidebar coin={bakedCoin} detail={baked} priceUSD="0.9998" />,
    );
    expect(await findByText('$3K')).toBeTruthy();
    expect(queryByText('$5M')).toBeNull();
    expect(queryByText('$8M')).toBeNull();
  });

  function staleRow(row: Record<string, unknown>) {
    vi.mocked(apiGet).mockImplementation(async (path: string) => {
      if (path === `/v1/assets/${encodeURIComponent(coin.asset_id)}`)
        return { data: row, flags: { stale: true } };
      throw new Error('offline');
    });
  }

  it('never puts the baked cap in place of one a stale live row withholds', async () => {
    staleRow({
      ...coin,
      market_cap_usd: null,
      market_cap_withheld_reason: 'supply_stale',
      volume_24h_usd: '3000',
    });
    const { findByText, getAllByText, queryByText } = renderWithQuery(
      <AssetSidebar coin={bakedCoin} detail={baked} priceUSD="0.9998" />,
    );
    expect(await findByText('$3K')).toBeTruthy();
    expect(queryByText('$5M')).toBeNull();
    expect(queryByText('not valued · supply out of date')).toBeTruthy();
    expect(getAllByText('stale · some reads out of date')).toHaveLength(2);
    expect(queryByText('as baked at deploy')).toBeNull();
  });

  it('keeps the labelled baked figures when a stale live row carries none', async () => {
    staleRow({ asset_id: coin.asset_id, observation_count: 0 });
    const { getAllByText, getByText } = renderWithQuery(
      <AssetSidebar coin={bakedCoin} detail={baked} priceUSD="0.9998" />,
    );
    await waitFor(() => expect(apiGet).toHaveBeenCalled());
    await new Promise((r) => setTimeout(r, 0));
    expect(getByText('$5M')).toBeTruthy();
    expect(getAllByText('as baked at deploy')).toHaveLength(2);
  });

  it('labels the baked figures when the live read fails', async () => {
    liveRow(null);
    const { getAllByText, getByText } = renderWithQuery(
      <AssetSidebar coin={bakedCoin} detail={baked} priceUSD="0.9998" />,
    );
    await waitFor(() => expect(apiGet).toHaveBeenCalled());
    expect(getByText('$5M')).toBeTruthy();
    expect(getByText('1.5K')).toBeTruthy();
    // Both the Market and the Activity panel carry the caption.
    expect(getAllByText('as baked at deploy')).toHaveLength(2);
  });
});
