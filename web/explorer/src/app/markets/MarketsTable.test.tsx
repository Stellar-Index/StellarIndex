import { describe, it, expect, vi } from 'vitest';
import { render, screen } from '@testing-library/react';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';

import { MarketsTable } from './MarketsTable';
import type { Market } from '@/api/hooks';

// UXP-10/UXP-16: /markets fetches (and the search box only filters) the
// top 100 pairs by 24h volume — thousands more trade on Stellar in any
// 14-day window. The search UI must say so instead of reading like a
// full-network search.
const markets: Market[] = Array.from({ length: 100 }, (_, i) => ({
  base: 'native',
  quote: `PAIR${i}-GABCDEFGHIJKLMNOPQRSTUVWXYZ234567ABCDEFGHIJKLMNOPQRSTUVWXY`,
  trade_count_24h: 10,
  last_trade_at: new Date().toISOString(),
})) as Market[];

vi.mock('@/api/hooks', async () => {
  const actual = await vi.importActual<typeof import('@/api/hooks')>('@/api/hooks');
  return {
    ...actual,
    useMarkets: () => ({ data: { markets }, isLoading: false, isError: false, error: null }),
  };
});

describe('MarketsTable', () => {
  it('tells the visitor the search only covers the top-N pairs shown, not every active pair', () => {
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    render(
      <QueryClientProvider client={client}>
        <MarketsTable />
      </QueryClientProvider>,
    );
    const input = screen.getByPlaceholderText(/Filter these 100 pairs/);
    expect(input).toBeInTheDocument();
    expect(input).toHaveAttribute('aria-label', expect.stringMatching(/does not search beyond/));
  });
});
