import { describe, it, expect, vi } from 'vitest';
import { render, screen, fireEvent } from '@testing-library/react';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';

vi.mock('@/api/client', async () => {
  const actual =
    await vi.importActual<typeof import('@/api/client')>('@/api/client');
  return { ...actual, apiGet: vi.fn() };
});

import { apiGet } from '@/api/client';
import { AssetSwap } from './AssetSwap';

// RLT-384. The converter's fiat legs come from /v1/price/batch, which
// this widget typed as `{data: Array<{asset_id, price}>}` — so
// `price_type` and `observed_at` were dropped and every fiat leg looked
// like a fresh observed quote. It is not a theoretical gap: measured on
// the live API 2026-09-19, fiat:EUR/fiat:USD answered `observed_at:
// 2026-09-18T00:00:00Z` (~36h old) with `flags.stale: true`, and the
// widget's whole output is a money amount computed from it.

function hoursAgo(h: number): string {
  return new Date(Date.now() - h * 3_600_000).toISOString();
}

function stubApi() {
  vi.mocked(apiGet).mockImplementation(async (path: string) => {
    if (path.startsWith('/v1/price/batch')) {
      return {
        data: [
          {
            asset_id: 'fiat:EUR',
            quote: 'fiat:USD',
            price: '1.148765077541643',
            price_type: 'vwap',
            observed_at: hoursAgo(36),
          },
        ],
        as_of: new Date().toISOString(),
        sources: ['massive'],
        flags: { stale: true },
      };
    }
    // The asset catalogue behind the picker's crypto rows: empty, so the
    // list under test is exactly the forex batch's.
    return { data: [], pagination: { next: '' } };
  });
}

function renderSwap() {
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  });
  return render(
    <QueryClientProvider client={client}>
      <AssetSwap symbol="XLM" assetId="native" priceUSD={0.195} />
    </QueryClientProvider>,
  );
}

/** Opens the "receive" leg's picker and chooses EUR. */
async function pickEURAsReceiveLeg() {
  const usdButton = await screen.findByRole('button', { name: /USD/ });
  fireEvent.click(usdButton);
  const eur = await screen.findByRole('button', { name: /EUR/ });
  fireEvent.click(eur);
}

// T303. useFiatTokens ran its /v1/price/batch fetch (and 5-minute poll)
// unconditionally on mount, before the picker that is the only place fiat
// rates are shown was ever opened.
describe('AssetSwap fiat batch fetch', () => {
  it('does not fetch fiat rates until the token picker is opened', async () => {
    stubApi();
    renderSwap();

    // Let any unconditional effect/query fire before we check.
    await screen.findByRole('button', { name: /USD/ });

    const fiatCalls = vi
      .mocked(apiGet)
      .mock.calls.filter(([path]) => path.startsWith('/v1/price/batch'));
    expect(fiatCalls).toHaveLength(0);

    await pickEURAsReceiveLeg();

    const fiatCallsAfterOpen = vi
      .mocked(apiGet)
      .mock.calls.filter(([path]) => path.startsWith('/v1/price/batch'));
    expect(fiatCallsAfterOpen.length).toBeGreaterThan(0);
  });
});

describe('AssetSwap fiat leg basis', () => {
  it('says when the fiat leg was observed instead of implying it is current', async () => {
    stubApi();
    renderSwap();
    await pickEURAsReceiveLeg();

    expect(
      await screen.findByText(/EUR price: observed 2d ago/),
    ).toBeInTheDocument();
  });

  it('still converts through the leg it is now describing', async () => {
    stubApi();
    renderSwap();
    await pickEURAsReceiveLeg();

    // 1 XLM × 0.195 USD ÷ 1.148765077541643 USD/EUR = 0.169745… EUR
    expect(await screen.findByDisplayValue(/^0\.1697/)).toBeInTheDocument();
  });
});
