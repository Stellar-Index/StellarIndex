import { describe, it, expect, vi } from 'vitest';
import { render, screen, waitFor } from '@testing-library/react';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';

import { SourceHealthPanel } from './SourceHealthPanel';

vi.mock('@/api/client', async () => {
  const actual =
    await vi.importActual<typeof import('@/api/client')>('@/api/client');
  return { ...actual, apiGet: vi.fn() };
});

import { apiGet } from '@/api/client';

const row = {
  name: 'sdex',
  class: 'exchange',
  enabled: true,
  entries_24h: 90000,
  trade_count_24h: 41000,
  markets_count_24h: 12,
  volume_24h_usd: '3200000',
};

function renderPanel() {
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  });
  render(
    <QueryClientProvider client={client}>
      <SourceHealthPanel source="sdex" />
    </QueryClientProvider>,
  );
}

// The API keeps its last good ingestion snapshot when a rebuild fails and
// flags it stale; its counters date from as_of, not the trailing 24h now.
describe('SourceHealthPanel staleness', () => {
  it('marks a stale snapshot as last known, with its age', async () => {
    const asOf = new Date(Date.now() - 3 * 3600_000).toISOString();
    vi.mocked(apiGet).mockResolvedValue({
      data: row,
      as_of: asOf,
      flags: { stale: true },
    });
    renderPanel();
    await waitFor(() =>
      expect(screen.getByText('Last known, not current')).toBeInTheDocument(),
    );
    expect(screen.getByText(/these figures are from 3h/)).toBeInTheDocument();
  });

  it('shows no staleness notice on a current snapshot', async () => {
    vi.mocked(apiGet).mockResolvedValue({
      data: row,
      as_of: new Date().toISOString(),
      flags: {},
    });
    renderPanel();
    // The stat grid renders only once the row has arrived.
    await waitFor(() => expect(screen.getByText('Trades')).toBeInTheDocument());
    expect(screen.queryByText('Last known, not current')).toBeNull();
  });
});
