import { describe, it, expect } from 'vitest';
import { render } from '@testing-library/react';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import type { ReactElement } from 'react';

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
