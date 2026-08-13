import { describe, it, expect, vi } from 'vitest';
import { render, screen, waitFor } from '@testing-library/react';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';

vi.mock('@/api/client', async () => {
  const actual = await vi.importActual<typeof import('@/api/client')>('@/api/client');
  return { ...actual, apiGet: vi.fn() };
});

import { apiGet } from '@/api/client';
import { PoolsTable } from './PoolsTable';

// Frontend-honesty sweep: /v1/markets answers 503 on its documented 8s
// trades-hypertable ceiling. Pre-fix the failed query coalesced to `[]`
// and fell into the empty state, publishing "No pools found in the last
// 14 days" — a false empirical claim about the venue. Absent must read
// as unavailable; a present-and-empty page still reads as "no pools".
describe('PoolsTable', () => {
  function renderTable() {
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    return render(
      <QueryClientProvider client={client}>
        <PoolsTable source="soroswap" sourceName="soroswap" />
      </QueryClientProvider>,
    );
  }

  it('renders the unavailable state, not "no pools", when the query fails', async () => {
    vi.mocked(apiGet).mockRejectedValue(new Error('HTTP 503'));
    renderTable();
    await waitFor(() =>
      expect(screen.getByText(/Pool list unavailable right now/)).toBeInTheDocument(),
    );
    expect(screen.queryByText(/No pools found in the last 14 days/)).not.toBeInTheDocument();
    // The row counter must not assert "0 on this page" either.
    expect(screen.getByText(/— on this page/)).toBeInTheDocument();
    expect(screen.queryByText(/0 on this page/)).not.toBeInTheDocument();
  });

  it('renders the genuine empty state when the API returns an empty page', async () => {
    vi.mocked(apiGet).mockResolvedValue({ data: [] });
    renderTable();
    await waitFor(() =>
      expect(screen.getByText(/No pools found in the last 14 days/)).toBeInTheDocument(),
    );
    expect(screen.queryByText(/Pool list unavailable/)).not.toBeInTheDocument();
    expect(screen.getByText(/0 on this page/)).toBeInTheDocument();
  });

  it('renders the rows when the API returns pools', async () => {
    vi.mocked(apiGet).mockResolvedValue({
      data: [
        {
          base: 'native',
          quote: 'USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN',
          last_trade_at: '2026-08-12T00:00:00Z',
          trade_count_24h: 12,
          volume_24h_usd: '4200',
          last_price: '0.42',
        },
      ],
    });
    renderTable();
    await waitFor(() => expect(screen.getByText(/1 on this page/)).toBeInTheDocument());
    expect(screen.queryByText(/Pool list unavailable/)).not.toBeInTheDocument();
    expect(screen.queryByText(/No pools found/)).not.toBeInTheDocument();
  });
});
