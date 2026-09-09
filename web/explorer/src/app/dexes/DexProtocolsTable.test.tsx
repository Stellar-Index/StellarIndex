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
          {
            name: 'soroswap',
            class: 'exchange',
            subclass: 'dex',
            trade_count_24h: 10,
            volume_24h_usd: '5000',
          },
          {
            name: 'phoenix',
            class: 'exchange',
            subclass: 'dex',
            trade_count_24h: 2,
            volume_24h_usd: '100',
          },
        ],
      };
    });

    const client = new QueryClient({
      defaultOptions: { queries: { retry: false } },
    });
    render(
      <QueryClientProvider client={client}>
        <DexProtocolsTable />
      </QueryClientProvider>,
    );

    expect(
      await screen.findByRole('columnheader', { name: 'TVL' }),
    ).toBeInTheDocument();
    // Lower-bound marker + compact formatting + basis as the hover title.
    await waitFor(() =>
      expect(screen.getByText(/≥\s*\$2\.5M/)).toBeInTheDocument(),
    );
    expect(screen.getByText(/≥\s*\$2\.5M/)).toHaveAttribute(
      'title',
      'current pair reserves',
    );
    // phoenix has no snapshot → its TVL cell is an em-dash (the row
    // renders one for TVL; its other cells hold real numbers).
    const phoenixRow = screen
      .getByRole('link', { name: 'phoenix' })
      .closest('tr');
    expect(phoenixRow?.textContent).toContain('—');
  });
});

// Frontend-honesty sweep: a failed /v1/sources fetch coalesced to `[]`
// and fell into the empty state — "No DEX protocols reporting 24h
// activity", i.e. the claim that Stellar has no active DEXes. Absent
// must read as unavailable; a genuinely empty answer still reads as the
// no-activity claim.
describe('DexProtocolsTable availability', () => {
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

  it('renders the unavailable state, not "no DEX protocols", when the fetch fails', async () => {
    vi.mocked(apiGet).mockRejectedValue(new Error('HTTP 503'));
    renderTable();
    await waitFor(() =>
      expect(
        screen.getByText(/Protocol list unavailable right now/),
      ).toBeInTheDocument(),
    );
    expect(
      screen.queryByText(/No DEX protocols reporting 24h activity/),
    ).not.toBeInTheDocument();
  });

  it('renders the genuine empty state when the API answers with no rows', async () => {
    vi.mocked(apiGet).mockResolvedValue({ data: [] });
    renderTable();
    await waitFor(() =>
      expect(
        screen.getByText(/No DEX protocols reporting 24h activity/),
      ).toBeInTheDocument(),
    );
    expect(
      screen.queryByText(/Protocol list unavailable/),
    ).not.toBeInTheDocument();
  });
});

// The headline total. /dexes fetched /v1/protocols for its TVL column
// and DISCARDED `tvl_total`, so the site's DEX page — the page the
// "DEX TVL" promise names — carried per-protocol figures and no
// headline. It renders now, under the same reconciliation rule the
// /protocols panel applies: the headline is the exact sum of the
// column beneath it, so it may only appear when every protocol it sums
// is a visible row that shows a figure.
describe('DexProtocolsTable headline total', () => {
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

  function mockAPI(opts: { total?: unknown; phoenixTvl?: boolean }) {
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
      return {
        data: [
          {
            name: 'soroswap',
            class: 'exchange',
            subclass: 'dex',
            trade_count_24h: 10,
            volume_24h_usd: '5000',
          },
          {
            name: 'phoenix',
            class: 'exchange',
            subclass: 'dex',
            trade_count_24h: 2,
            volume_24h_usd: '100',
          },
        ],
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

  it('renders the served tvl_total with its lower-bound marker and basis', async () => {
    mockAPI({ total, phoenixTvl: true });
    renderTable();

    const label = await screen.findByText('Total value locked');
    // Scoped to the headline card: the table's own TVL cells carry a
    // "≥" too, and an unscoped match would pass on either.
    const card = label.parentElement as HTMLElement;
    // Money comes off the DECIMAL STRING, grouped — never Number().
    await waitFor(() =>
      expect(card.textContent).toMatch(/≥\s*\$2,600,000\.00/),
    );
    // Lower bound: the priced/total split, so the headline degrades
    // exactly the way its parts do.
    expect(screen.getByText(/40 of 42 pools priced/)).toBeInTheDocument();
    expect(screen.getByText(new RegExp(total.basis))).toBeInTheDocument();
    // Scope is reachable from the surface, not hidden behind a hover.
    expect(screen.getByText(/What this total excludes/)).toBeInTheDocument();
  });

  it('withholds the headline when a summed protocol is not in the table', async () => {
    // phoenix is summed by the server but carries no snapshot here, so
    // its TVL cell is an em-dash. Adding the visible column would not
    // reach 2,600,000 — so no headline at all, never a number that
    // does not reconcile with what is on screen.
    mockAPI({ total, phoenixTvl: false });
    renderTable();

    expect(
      await screen.findByRole('columnheader', { name: 'TVL' }),
    ).toBeInTheDocument();
    await waitFor(() =>
      expect(screen.getByText(/\$2\.5M/)).toBeInTheDocument(),
    );
    expect(screen.queryByText('Total value locked')).not.toBeInTheDocument();
    expect(screen.queryByText(/\$2,600,000\.00/)).not.toBeInTheDocument();
  });

  it('renders no headline and no zero when the server omits tvl_total', async () => {
    // tvl_total is omitempty: absent when the reconciliation could
    // admit nothing. Absent must stay absent — "$0.00" beside non-zero
    // rows is the one reading that is definitely wrong.
    mockAPI({ phoenixTvl: true });
    renderTable();

    expect(
      await screen.findByRole('columnheader', { name: 'TVL' }),
    ).toBeInTheDocument();
    await waitFor(() =>
      expect(screen.getByText(/\$2\.5M/)).toBeInTheDocument(),
    );
    expect(screen.queryByText('Total value locked')).not.toBeInTheDocument();
    expect(screen.queryByText(/\$0\.00/)).not.toBeInTheDocument();
  });
});
