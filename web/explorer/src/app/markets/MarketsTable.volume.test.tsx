import { describe, it, expect, vi } from 'vitest';
import { render, screen } from '@testing-library/react';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';

import { MarketsTable } from './MarketsTable';
import type { Market } from '@/api/hooks';

const markets = [
  {
    base: 'native',
    quote: 'USDC-GABCDEFGHIJKLMNOPQRSTUVWXYZ234567ABCDEFGHIJKLMNOPQRSTUVWXY',
    trade_count_24h: 10,
    volume_24h_usd: '1000000004999999999',
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

describe('MarketsTable volume', () => {
  it('rounds volume above 2^53 from the exact decimal', () => {
    render(
      <QueryClientProvider client={new QueryClient()}>
        <MarketsTable />
      </QueryClientProvider>,
    );
    expect(screen.getAllByText('$1,000,000T').length).toBeGreaterThan(0);
  });
});
