import { describe, it, expect, vi } from 'vitest';
import { render, screen, waitFor } from '@testing-library/react';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';

vi.mock('@/api/client', async () => {
  const actual = await vi.importActual<typeof import('@/api/client')>('@/api/client');
  return { ...actual, apiGet: vi.fn() };
});

import { apiGet } from '@/api/client';
import { ReferencePriceAggregators } from './ReferencePriceAggregators';

// Frontend-honesty sweep: a failed /v1/sources fetch coalesced to `[]`
// and claimed "No reference aggregators registered."
describe('ReferencePriceAggregators', () => {
  function renderPanel() {
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    return render(
      <QueryClientProvider client={client}>
        <ReferencePriceAggregators />
      </QueryClientProvider>,
    );
  }

  it('renders the unavailable state, not "none registered", when the fetch fails', async () => {
    vi.mocked(apiGet).mockRejectedValue(new Error('HTTP 503'));
    renderPanel();
    await waitFor(() =>
      expect(screen.getByText(/Source registry unavailable right now/)).toBeInTheDocument(),
    );
    expect(screen.queryByText(/No reference aggregators registered/)).not.toBeInTheDocument();
  });

  it('renders the genuine empty state when the API answers with no rows', async () => {
    vi.mocked(apiGet).mockResolvedValue({ data: [] });
    renderPanel();
    await waitFor(() =>
      expect(screen.getByText(/No reference aggregators registered/)).toBeInTheDocument(),
    );
    expect(screen.queryByText(/Source registry unavailable/)).not.toBeInTheDocument();
  });
});
