import { describe, it, expect, vi } from 'vitest';
import { render, screen } from '@testing-library/react';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';

vi.mock('@/api/client', async () => {
  const actual =
    await vi.importActual<typeof import('@/api/client')>('@/api/client');
  return { ...actual, apiGet: vi.fn() };
});

import { apiGet } from '@/api/client';
import { ConvertLiveRate, ConvertSnippets } from './ConvertLive';

// W8 recon 10a: under static export only ConvertPair re-fetched, so the
// /convert header, inverse, and common-amounts ladder stayed frozen at
// the BUILD-baked rate while the copy claimed "current mid-market rate".
// These tests pin the corrected behaviour: the baked value paints first
// (RT-2), then every "current"-labelled element hydrates to the LIVE
// /v1/price/batch rate — not the stale baked prop.
//
// The build baked 1 USD = 0.85 EUR; the live wire says 1 EUR = 2 USD,
// i.e. 1 USD = 0.50 EUR. Post-fix the page must show 0.50, never 0.85.
function stubLiveRate() {
  // batch(asset_ids=fiat:EUR, quote=fiat:USD) → value of 1 EUR in USD
  // units. price '2' ⇒ 1 EUR = 2 USD ⇒ live 1 USD = 0.5 EUR (inverted).
  vi.mocked(apiGet).mockResolvedValue({
    data: [{ asset_id: 'fiat:EUR', price: '2' }],
  });
}

function renderWithQuery(ui: React.ReactElement) {
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  });
  return render(
    <QueryClientProvider client={client}>{ui}</QueryClientProvider>,
  );
}

const BAKED_RATE = 0.85; // 1 USD = 0.85 EUR baked at build
const BAKED_INVERSE = 1 / BAKED_RATE; // ≈ 1.1765

describe('ConvertLiveRate', () => {
  it('paints the baked rate first, then hydrates the header + inverse to the LIVE rate', async () => {
    stubLiveRate();
    renderWithQuery(
      <ConvertLiveRate
        from="USD"
        to="EUR"
        initialRate={BAKED_RATE}
        initialInverse={BAKED_INVERSE}
      />,
    );

    // First paint = the build-baked value (SSR-correct, crawler-visible).
    expect(screen.getByText(/0\.850000/)).toBeInTheDocument();

    // …then the client re-fetches and the headline becomes the LIVE
    // rate: 1 USD = 0.50 EUR, and the inverse becomes 1 EUR = 2.0000 USD.
    expect(await screen.findByText(/0\.500000/)).toBeInTheDocument();
    expect(screen.getByText(/2\.0000/)).toBeInTheDocument();

    // The stale baked figures must be gone once hydrated.
    expect(screen.queryByText(/0\.850000/)).not.toBeInTheDocument();
    expect(screen.queryByText(/1\.1765/)).not.toBeInTheDocument();
  });
});

describe('ConvertSnippets', () => {
  it('paints the baked ladder first, then hydrates every amount + the caption to the LIVE rate', async () => {
    stubLiveRate();
    renderWithQuery(
      <ConvertSnippets
        from="USD"
        to="EUR"
        initialRate={BAKED_RATE}
        initialInverse={BAKED_INVERSE}
      />,
    );

    // First paint: the 100-USD row + caption at the baked rate
    // (exact match so baked 850.0000 for the 1000-row can't shadow it).
    expect(
      screen.getByText('85.0000 EUR', { exact: true }),
    ).toBeInTheDocument();
    expect(
      screen.getByText(/mid-market rate of 1 USD = 0\.850000 EUR/),
    ).toBeInTheDocument();

    // …then wait for the caption to hydrate to the LIVE "current" rate,
    // and confirm the ladder row tracks it: 100 × 0.50 = 50.0000 EUR.
    expect(
      await screen.findByText(/mid-market rate of 1 USD = 0\.500000 EUR/),
    ).toBeInTheDocument();
    expect(
      screen.getByText('50.0000 EUR', { exact: true }),
    ).toBeInTheDocument();

    // The stale baked ladder row + caption must be gone once hydrated.
    expect(
      screen.queryByText('85.0000 EUR', { exact: true }),
    ).not.toBeInTheDocument();
    expect(
      screen.queryByText(/mid-market rate of 1 USD = 0\.850000 EUR/),
    ).not.toBeInTheDocument();
  });
});
