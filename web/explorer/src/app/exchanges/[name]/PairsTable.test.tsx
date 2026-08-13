import { describe, it, expect, vi } from 'vitest';
import { render, screen, waitFor } from '@testing-library/react';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';

vi.mock('@/api/client', async () => {
  const actual = await vi.importActual<typeof import('@/api/client')>('@/api/client');
  return { ...actual, apiGet: vi.fn() };
});

import { apiGet } from '@/api/client';
import { PairsTable } from './PairsTable';

// Frontend-honesty sweep: a /v1/markets timeout used to render as
// "No pairs found in the last 14 days" — i.e. "this exchange traded
// nothing for two weeks". Absent ≠ empty.
describe('PairsTable', () => {
  function renderTable() {
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    return render(
      <QueryClientProvider client={client}>
        <PairsTable source="binance" exchangeName="Binance" />
      </QueryClientProvider>,
    );
  }

  it('renders the unavailable state, not "no pairs", when the query fails', async () => {
    vi.mocked(apiGet).mockRejectedValue(new Error('HTTP 503'));
    renderTable();
    await waitFor(() =>
      expect(screen.getByText(/Pair list unavailable right now/)).toBeInTheDocument(),
    );
    expect(screen.queryByText(/No pairs found in the last 14 days/)).not.toBeInTheDocument();
    expect(screen.getByText(/— on this page/)).toBeInTheDocument();
  });

  it('renders the genuine empty state when the API returns an empty page', async () => {
    vi.mocked(apiGet).mockResolvedValue({ data: [] });
    renderTable();
    await waitFor(() =>
      expect(screen.getByText(/No pairs found in the last 14 days/)).toBeInTheDocument(),
    );
    expect(screen.queryByText(/Pair list unavailable/)).not.toBeInTheDocument();
    expect(screen.getByText(/0 on this page/)).toBeInTheDocument();
  });
});
