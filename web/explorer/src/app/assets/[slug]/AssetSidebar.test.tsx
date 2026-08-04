import { describe, it, expect } from 'vitest';
import { render } from '@testing-library/react';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import type { ReactElement } from 'react';

import { AssetSidebar, type SidebarCoin } from './AssetSidebar';

// AssetSidebar embeds ChangeSummaryStrip (a TanStack Query consumer),
// so renders need a QueryClient. Retries off — a test must not poll.
function renderWithQuery(ui: ReactElement) {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(<QueryClientProvider client={qc}>{ui}</QueryClientProvider>);
}

// 2026-08-04: the headline price must SAY what it is. Pre-fix the
// sidebar rendered a bare dollar figure whether it came from the live
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

describe('AssetSidebar price provenance caption', () => {
  it('labels a live closed-bucket price as 1-min VWAP', () => {
    const { container } = renderWithQuery(
      <AssetSidebar coin={coin} detail={null} priceUSD={0.9998} priceProvenance="vwap1m" />,
    );
    expect(container.textContent).toContain('1-min VWAP · USD');
  });

  it('labels a triangulated price as such', () => {
    const { container } = renderWithQuery(
      <AssetSidebar coin={coin} detail={null} priceUSD={0.9998} priceProvenance="triangulated" />,
    );
    expect(container.textContent).toContain('triangulated via XLM');
  });

  it('labels the build-time listing fallback as not-live', () => {
    const { container } = renderWithQuery(
      <AssetSidebar coin={coin} detail={null} priceUSD={0.9998} priceProvenance="listing" />,
    );
    expect(container.textContent).toContain('listing snapshot');
    expect(container.textContent).toContain('not a live aggregated price');
  });

  it('marks a stale price', () => {
    const { container } = renderWithQuery(
      <AssetSidebar coin={coin} detail={null} priceUSD={0.9998} priceProvenance="vwap1m" priceStale />,
    );
    expect(container.textContent).toContain('stale');
  });

  it('renders no caption when there is no price', () => {
    const { container } = renderWithQuery(
      <AssetSidebar coin={coin} detail={null} priceUSD={null} priceProvenance={null} />,
    );
    expect(container.textContent).not.toContain('VWAP');
    expect(container.textContent).not.toContain('listing snapshot');
  });
});
