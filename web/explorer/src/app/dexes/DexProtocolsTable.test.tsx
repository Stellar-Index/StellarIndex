import { describe, it, expect, vi } from 'vitest';
import { render, screen, waitFor } from '@testing-library/react';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';

import { DexProtocolsTable } from './DexProtocolsTable';

vi.mock('@/api/client', async () => {
  const actual = await vi.importActual<typeof import('@/api/client')>('@/api/client');
  return { ...actual, apiGet: vi.fn() };
});

import { apiGet } from '@/api/client';

// TVL join: the per-protocol snapshot from /v1/protocols renders as a
// TVL cell; a snapshot with unpriced pools shows the "≥" lower-bound
// marker; protocols without a snapshot entry render an em-dash, never
// a fabricated $0.
describe('DexProtocolsTable TVL column', () => {
  it('renders TVL from /v1/protocols, lower-bound marked, absent as —', async () => {
    vi.mocked(apiGet).mockImplementation(async (path: string) => {
      if (path === '/v1/protocols') {
        return {
          data: {
            protocols: [
              {
                name: 'soroswap',
                category: 'amm',
                tvl: {
                  tvl_usd: '2500000.00',
                  pools_total: 40,
                  pools_priced: 38,
                  unpriced_pools: 2,
                  as_of: '2026-07-29T00:00:00Z',
                  basis: 'current pair reserves',
                },
              },
              { name: 'phoenix', category: 'amm' },
            ],
            total_protocols: 2,
          },
        };
      }
      // /v1/sources
      return {
        data: [
          { name: 'soroswap', class: 'exchange', subclass: 'dex', trade_count_24h: 10, volume_24h_usd: '5000' },
          { name: 'phoenix', class: 'exchange', subclass: 'dex', trade_count_24h: 2, volume_24h_usd: '100' },
        ],
      };
    });

    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    render(
      <QueryClientProvider client={client}>
        <DexProtocolsTable />
      </QueryClientProvider>,
    );

    expect(await screen.findByRole('columnheader', { name: 'TVL' })).toBeInTheDocument();
    // Lower-bound marker + compact formatting + basis as the hover title.
    await waitFor(() => expect(screen.getByText(/≥\s*\$2\.5M/)).toBeInTheDocument());
    expect(screen.getByText(/≥\s*\$2\.5M/)).toHaveAttribute('title', 'current pair reserves');
    // phoenix has no snapshot → its TVL cell is an em-dash (the row
    // renders one for TVL; its other cells hold real numbers).
    const phoenixRow = screen.getByRole('link', { name: 'phoenix' }).closest('tr');
    expect(phoenixRow?.textContent).toContain('—');
  });
});
