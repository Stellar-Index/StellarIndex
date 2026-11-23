import { describe, it, expect, vi } from 'vitest';
import { render, screen } from '@testing-library/react';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';

import { SourceCursorsPanel } from './SourceCursorsPanel';

vi.mock('@/api/client', async () => {
  const actual =
    await vi.importActual<typeof import('@/api/client')>('@/api/client');
  return { ...actual, apiGet: vi.fn() };
});

import { apiGet } from '@/api/client';

function renderPanel(source: string) {
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  });
  render(
    <QueryClientProvider client={client}>
      <SourceCursorsPanel source={source} />
    </QueryClientProvider>,
  );
}

describe('SourceCursorsPanel', () => {
  it('renders the live lag of this venue’s cursors only', async () => {
    vi.mocked(apiGet).mockResolvedValue({
      data: [
        {
          source: 'projector',
          sub_source: 'soroswap',
          last_ledger: 100,
          last_updated: '2026-11-23T10:00:00Z',
          lag_seconds: 7200,
        },
        {
          source: 'backfill',
          sub_source: '1-50:sdex',
          last_ledger: 50,
          last_updated: '2026-11-23T10:00:00Z',
          lag_seconds: 4,
        },
      ],
    });
    renderPanel('soroswap');
    expect(await screen.findByText('2.0h')).toBeInTheDocument();
    expect(screen.queryByText('1-50:sdex')).not.toBeInTheDocument();
    expect(screen.getByText(/^1 entries/)).toBeInTheDocument();
  });

  it('says the cursors are unavailable on a failed read, not "none recorded"', async () => {
    vi.mocked(apiGet).mockRejectedValue(new Error('503'));
    renderPanel('soroswap');
    expect(
      await screen.findByText('Ingest cursors unavailable right now.'),
    ).toBeInTheDocument();
    expect(
      screen.queryByText('No cursor recorded for this source.'),
    ).not.toBeInTheDocument();
    expect(screen.getByText(/^— entries/)).toBeInTheDocument();
  });
});
