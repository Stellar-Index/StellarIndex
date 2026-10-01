import { describe, it, expect, vi } from 'vitest';
import { render, screen, waitFor } from '@testing-library/react';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';

vi.mock('@/api/client', async () => {
  const actual =
    await vi.importActual<typeof import('@/api/client')>('@/api/client');
  return { ...actual, apiGet: vi.fn() };
});

import { apiGet } from '@/api/client';
import { ExchangesView } from './ExchangesView';

// Frontend-honesty sweep: the registry table coalesced a failed fetch to
// `[]` and claimed "No CEX sources reporting.". Absent must read as
// unavailable.
describe('ExchangesView', () => {
  function renderView() {
    const client = new QueryClient({
      defaultOptions: { queries: { retry: false } },
    });
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
      expect(
        screen.getByText(/Exchange registry unavailable right now/),
      ).toBeInTheDocument(),
    );
    expect(
      screen.queryByText(/No CEX sources reporting/),
    ).not.toBeInTheDocument();
    // The panel headings must not assert a count either.
    expect(
      screen.queryByText(/0 centralised exchanges/),
    ).not.toBeInTheDocument();
  });

  it('renders the genuine empty states when the API answers with no rows', async () => {
    vi.mocked(apiGet).mockResolvedValue({ data: [] });
    renderView();
    await waitFor(() =>
      expect(screen.getByText(/No CEX sources reporting/)).toBeInTheDocument(),
    );
    expect(screen.queryByText(/unavailable right now/)).not.toBeInTheDocument();
    expect(screen.getByText(/0 centralised exchanges/)).toBeInTheDocument();
  });

  // A CEX's markets are never fetched on their own: /v1/markets refuses an
  // off-chain source filter, and the page must not depend on one.
  it("never requests a single venue's markets", async () => {
    vi.mocked(apiGet).mockReset();
    vi.mocked(apiGet).mockResolvedValue({
      data: [{ name: 'binance', class: 'exchange', subclass: 'cex' }],
    });
    renderView();
    await waitFor(() =>
      expect(screen.getByText(/1 centralised exchange/)).toBeInTheDocument(),
    );
    const sourceScoped = vi
      .mocked(apiGet)
      .mock.calls.filter(
        ([p, q]) =>
          p === '/v1/markets' && (q as { source?: string } | undefined)?.source,
      );
    expect(sourceScoped).toEqual([]);
  });
});
