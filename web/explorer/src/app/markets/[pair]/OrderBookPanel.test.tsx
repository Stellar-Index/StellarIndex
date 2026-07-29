import { describe, it, expect, vi } from 'vitest';
import { render, screen, waitFor } from '@testing-library/react';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';

import { OrderBookPanel, isClassicAssetId, trimTrailingZeros } from './OrderBookPanel';

vi.mock('@/api/client', async () => {
  const actual = await vi.importActual<typeof import('@/api/client')>('@/api/client');
  return { ...actual, apiGet: vi.fn() };
});

import { apiGet } from '@/api/client';

const USDC = 'USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN';

function renderPanel(base: string, quote: string) {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(
    <QueryClientProvider client={client}>
      <OrderBookPanel base={base} quote={quote} />
    </QueryClientProvider>,
  );
}

describe('OrderBookPanel', () => {
  it('renders bid/ask depth for a classic pair', async () => {
    vi.mocked(apiGet).mockResolvedValue({
      data: {
        selling: 'native',
        buying: USDC,
        as_of_ledger: 63412345,
        snapshot_at: '2026-07-29T12:34:56Z',
        asks: [
          {
            price: '0.5000000', price_r: { n: 1, d: 2 },
            base_amount: '10.0000000', quote_amount: '5.0000000',
            cum_base_amount: '10.0000000', cum_quote_amount: '5.0000000',
            offers: 2,
          },
        ],
        bids: [
          {
            price: '0.4900000', price_r: { n: 49, d: 100 },
            base_amount: '12.0000000', quote_amount: '5.8800000',
            cum_base_amount: '12.0000000', cum_quote_amount: '5.8800000',
            offers: 1,
          },
        ],
        ask_offers: 2,
        bid_offers: 1,
        depth: 12,
      },
    });

    renderPanel('native', USDC);
    expect(await screen.findByText('SDEX order book')).toBeInTheDocument();
    await waitFor(() => expect(screen.getByText('0.5')).toBeInTheDocument());
    expect(screen.getByText('0.49')).toBeInTheDocument();
    expect(screen.getByText('Bids')).toBeInTheDocument();
    expect(screen.getByText('Asks')).toBeInTheDocument();
    expect(screen.getByText(/as of ledger/)).toBeInTheDocument();
  });

  it('renders nothing for a Soroban pair and an honest state while the snapshot loads', async () => {
    const { container } = renderPanel(
      'CAS3J7GYLGXMF6TDJBBYYSE3HQ6BBSMLNUQ34T6TZMYMW2EVH34XOWMA',
      'native',
    );
    expect(container.firstChild).toBeNull();
    expect(apiGet).not.toHaveBeenCalled();

    vi.mocked(apiGet).mockRejectedValue(
      new Error('503 Service Unavailable on /v1/sdex/orderbook — Order book snapshot is loading'),
    );
    renderPanel('native', USDC);
    await waitFor(() =>
      expect(screen.getByText(/snapshot is still loading/)).toBeInTheDocument(),
    );
  });
});

describe('helpers', () => {
  it('isClassicAssetId accepts native + CODE-G..., rejects Soroban/fiat', () => {
    expect(isClassicAssetId('native')).toBe(true);
    expect(isClassicAssetId(USDC)).toBe(true);
    expect(isClassicAssetId('CAS3J7GYLGXMF6TDJBBYYSE3HQ6BBSMLNUQ34T6TZMYMW2EVH34XOWMA')).toBe(false);
    expect(isClassicAssetId('fiat:USD')).toBe(false);
    expect(isClassicAssetId('')).toBe(false);
  });

  it('trimTrailingZeros trims render noise without touching precision', () => {
    expect(trimTrailingZeros('10.5000000')).toBe('10.5');
    expect(trimTrailingZeros('0.5000000')).toBe('0.5');
    expect(trimTrailingZeros('42')).toBe('42');
    expect(trimTrailingZeros('3.0000000')).toBe('3');
  });
});
