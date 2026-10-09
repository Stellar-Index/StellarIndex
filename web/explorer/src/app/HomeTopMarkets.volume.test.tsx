import { describe, it, expect, vi } from 'vitest';
import { render, screen } from '@testing-library/react';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';

import { HomeTopMarkets } from './HomeTopMarkets';
import type { Market } from '@/api/hooks';

const markets = [
  {
    base: 'native',
    quote: 'USDC-GABCDEFGHIJKLMNOPQRSTUVWXYZ234567ABCDEFGHIJKLMNOPQRSTUVWXY',
    trade_count_24h: 10,
    volume_24h_usd: '1000000004999999999',
    last_trade_at: new Date().toISOString(),
  },
  {
    base: 'native',
    quote: 'EURC-GABCDEFGHIJKLMNOPQRSTUVWXYZ234567ABCDEFGHIJKLMNOPQRSTUVWXY',
    trade_count_24h: 4,
    volume_24h_usd: '500000002499999999',
    last_trade_at: new Date().toISOString(),
  },
] as Market[];

vi.mock('@/api/hooks', async () => {
  const actual =
    await vi.importActual<typeof import('@/api/hooks')>('@/api/hooks');
  return {
    ...actual,
    useMarkets: () => ({
      data: { markets },
      isLoading: false,
      isError: false,
      error: null,
    }),
  };
});

vi.mock('@/lib/live/hooks', async () => {
  const actual =
    await vi.importActual<typeof import('@/lib/live/hooks')>(
      '@/lib/live/hooks',
    );
  return { ...actual, useLedgerFollow: () => undefined };
});

describe('HomeTopMarkets volume', () => {
  it('rounds volume above 2^53 from the exact decimal', () => {
    render(
      <QueryClientProvider client={new QueryClient()}>
        <HomeTopMarkets />
      </QueryClientProvider>,
    );
    expect(screen.getByText('$1,000,000T')).toBeInTheDocument();
  });

  it('scales each volume bar to the top pair', () => {
    render(
      <QueryClientProvider client={new QueryClient()}>
        <HomeTopMarkets />
      </QueryClientProvider>,
    );
    const widths = screen
      .getAllByRole('img', { name: '24h volume relative to the top pair' })
      .map((bar) => (bar.firstElementChild as HTMLElement).style.width);
    expect(widths).toEqual(['100%', '50%']);
  });
});
