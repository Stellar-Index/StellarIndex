import { describe, it, expect, vi } from 'vitest';
import { render, screen, waitFor } from '@testing-library/react';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';

vi.mock('@/api/client', async () => {
  const actual = await vi.importActual<typeof import('@/api/client')>('@/api/client');
  return { ...actual, apiGet: vi.fn() };
});

import { apiGet } from '@/api/client';
import { ExchangesView } from './ExchangesView';

// Frontend-honesty sweep: both tables on /exchanges coalesced a failed
// fetch to `[]`. The registry table then claimed "No CEX sources
// reporting."; the pair table (a Promise.all over four venue-scoped
// /v1/markets calls, so ONE 503 rejects the lot) headlined "0 CEX pairs"
// and "No CEX pairs reporting.". Absent must read as unavailable.
describe('ExchangesView', () => {
  function renderView() {
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    return render(
      <QueryClientProvider client={client}>
        <ExchangesView />
      </QueryClientProvider>,
    );
  }

  it('renders unavailable states, not absence claims, when the fetches fail', async () => {
    vi.mocked(apiGet).mockRejectedValue(new Error('HTTP 503'));
    renderView();
    await waitFor(() =>
      expect(screen.getByText(/Exchange registry unavailable right now/)).toBeInTheDocument(),
    );
    expect(screen.getByText(/Pair list unavailable right now/)).toBeInTheDocument();
    expect(screen.queryByText(/No CEX sources reporting/)).not.toBeInTheDocument();
    expect(screen.queryByText(/No CEX pairs reporting/)).not.toBeInTheDocument();
    // The panel headings must not assert a count either.
    expect(screen.queryByText(/0 centralised exchanges/)).not.toBeInTheDocument();
    expect(screen.queryByText(/0 CEX pairs/)).not.toBeInTheDocument();
  });

  it('renders the genuine empty states when the API answers with no rows', async () => {
    vi.mocked(apiGet).mockResolvedValue({ data: [] });
    renderView();
    await waitFor(() =>
      expect(screen.getByText(/No CEX sources reporting/)).toBeInTheDocument(),
    );
    expect(screen.getByText(/No CEX pairs reporting/)).toBeInTheDocument();
    expect(screen.queryByText(/unavailable right now/)).not.toBeInTheDocument();
    expect(screen.getByText(/0 centralised exchanges/)).toBeInTheDocument();
  });
});
