import { describe, it, expect, vi } from 'vitest';
import { render, screen, waitFor } from '@testing-library/react';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';

import { DexProtocolsTable } from './DexProtocolsTable';

vi.mock('@/api/client', async () => {
  const actual =
    await vi.importActual<typeof import('@/api/client')>('@/api/client');
  return { ...actual, apiGet: vi.fn() };
});

import { apiGet } from '@/api/client';

const total = {
  tvl_usd: '2600000.00',
  protocols: ['phoenix', 'soroswap'],
  lower_bound: true,
  pools_total: 42,
  pools_priced: 40,
  unpriced_pools: 2,
  as_of_ledger: 58_400_100,
  as_of: '2026-07-29T00:00:00Z',
  basis:
    'exact sum of the published per-protocol tvl_usd for phoenix, soroswap',
  excluded: [
    { subject: 'sdex', reason: 'the classic order book holds offers' },
  ],
};

function mockAPI(opts: { total?: unknown; phoenixTvl?: boolean } = {}) {
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
            {
              name: 'phoenix',
              category: 'amm',
              ...(opts.phoenixTvl
                ? {
                    tvl: {
                      tvl_usd: '100000.00',
                      pools_total: 2,
                      pools_priced: 2,
                      unpriced_pools: 0,
                      as_of: '2026-07-29T00:00:00Z',
                      basis: 'current pool reserves',
                    },
                  }
                : {}),
            },
          ],
          total_protocols: 2,
          ...(opts.total ? { tvl_total: opts.total } : {}),
        },
      };
    }
    // /v1/sources
    return {
      data: [
        ['soroswap', 10, '5000'],
        ['phoenix', 2, '100'],
      ].map(([name, trades, vol]) => ({
        name,
        class: 'exchange',
        subclass: 'dex',
        trade_count_24h: trades,
        volume_24h_usd: vol,
      })),
    };
  });
}

function renderTable() {
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  });
  return render(
    <QueryClientProvider client={client}>
      <DexProtocolsTable />
    </QueryClientProvider>,
  );
}

describe('DexProtocolsTable TVL column', () => {
  it('renders TVL from /v1/protocols, lower-bound marked, absent as —', async () => {
    mockAPI();
    renderTable();

    expect(
      await screen.findByRole('columnheader', { name: 'TVL' }),
    ).toBeInTheDocument();
    // Lower-bound marker + compact formatting + basis as the hover title.
    expect(await screen.findByText(/≥\s*\$2\.5M/)).toBeInTheDocument();
    expect(screen.getByText(/≥\s*\$2\.5M/)).toHaveAttribute(
      'title',
      'current pair reserves',
    );
    // No snapshot renders an em-dash, never a fabricated $0.
    const phoenixRow = screen
      .getByRole('link', { name: 'phoenix' })
      .closest('tr');
    expect(phoenixRow?.textContent).toContain('—');
  });
});

// A failed /v1/sources fetch must not read as "Stellar has no active DEXes".
describe('DexProtocolsTable availability', () => {
  it.each([
    [
      'unavailable, not "no DEX protocols", when the fetch fails',
      () => vi.mocked(apiGet).mockRejectedValue(new Error('HTTP 503')),
      /Protocol list unavailable right now/,
      /No DEX protocols reporting 24h activity/,
    ],
    [
      'the genuine empty state when the API answers with no rows',
      () => vi.mocked(apiGet).mockResolvedValue({ data: [] }),
      /No DEX protocols reporting 24h activity/,
      /Protocol list unavailable/,
    ],
  ])('renders %s', async (_name, arrange, shown, hidden) => {
    arrange();
    renderTable();
    expect(await screen.findByText(shown)).toBeInTheDocument();
    expect(screen.queryByText(hidden)).not.toBeInTheDocument();
  });
});

// The headline is the exact sum of the column beneath it, so it may only
// appear when every protocol it sums is a visible row showing a figure.
describe('DexProtocolsTable headline total', () => {
  it('renders the served tvl_total with its lower-bound marker and basis', async () => {
    mockAPI({ total, phoenixTvl: true });
    renderTable();

    const label = await screen.findByText('Total value locked');
    // Scoped to the card: the table's own TVL cells carry a "≥" too.
    const card = label.parentElement as HTMLElement;
    // Money comes off the DECIMAL STRING, grouped; never Number().
    await waitFor(() =>
      expect(card.textContent).toMatch(/≥\s*\$2,600,000\.00/),
    );
    expect(screen.getByText(/40 of 42 pools priced/)).toBeInTheDocument();
    expect(screen.getByText(new RegExp(total.basis))).toBeInTheDocument();
    expect(screen.getByText(/What this total excludes/)).toBeInTheDocument();
  });

  it.each([
    [
      'a summed protocol (phoenix) is not in the table',
      { total, phoenixTvl: false },
      /\$2,600,000\.00/,
    ],
    ['the server omits tvl_total', { phoenixTvl: true }, /\$0\.00/],
  ])(
    'renders no headline and no stray figure when %s',
    async (_n, opts, bad) => {
      mockAPI(opts);
      renderTable();

      expect(
        await screen.findByRole('columnheader', { name: 'TVL' }),
      ).toBeInTheDocument();
      expect(await screen.findByText(/\$2\.5M/)).toBeInTheDocument();
      expect(screen.queryByText('Total value locked')).not.toBeInTheDocument();
      expect(screen.queryByText(bad)).not.toBeInTheDocument();
    },
  );
});
