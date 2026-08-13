import { describe, it, expect, vi } from 'vitest';
import { render, screen, waitFor } from '@testing-library/react';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';

vi.mock('@/api/client', async () => {
  const actual = await vi.importActual<typeof import('@/api/client')>('@/api/client');
  return { ...actual, apiGet: vi.fn() };
});

import { apiGet } from '@/api/client';
import { OraclesView } from './OraclesView';

// Frontend-honesty sweep: a failed /v1/sources or /v1/oracle/streams
// fetch coalesced to `[]` and rendered "No oracles registered." /
// "No oracle observations in the last 7 days." — empirical claims about
// the network made from a request that never answered.
describe('OraclesView', () => {
  function renderView() {
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    return render(
      <QueryClientProvider client={client}>
        <OraclesView />
      </QueryClientProvider>,
    );
  }

  it('renders unavailable states, not absence claims, when the fetches fail', async () => {
    vi.mocked(apiGet).mockRejectedValue(new Error('HTTP 503'));
    renderView();
    await waitFor(() =>
      expect(screen.getByText(/Oracle registry unavailable right now/)).toBeInTheDocument(),
    );
    expect(screen.getByText(/Oracle observations unavailable right now/)).toBeInTheDocument();
    expect(screen.queryByText(/No oracles registered/)).not.toBeInTheDocument();
    expect(
      screen.queryByText(/No oracle observations in the last 7 days/),
    ).not.toBeInTheDocument();
  });

  it('renders the genuine empty states when the API answers with no rows', async () => {
    vi.mocked(apiGet).mockResolvedValue({ data: [] });
    renderView();
    await waitFor(() => expect(screen.getByText(/No oracles registered/)).toBeInTheDocument());
    expect(screen.getByText(/No oracle observations in the last 7 days/)).toBeInTheDocument();
    expect(screen.queryByText(/unavailable right now/)).not.toBeInTheDocument();
  });
});
