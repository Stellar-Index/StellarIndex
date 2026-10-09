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

// Frontend-honesty sweep: both tables on /exchanges coalesced a failed
// fetch to `[]`. The registry table then claimed "No CEX sources
// reporting."; the pair table (a Promise.all over four venue-scoped
// /v1/markets calls, so ONE 503 rejects the lot) headlined "0 CEX pairs"
// and "No CEX pairs reporting.". Absent must read as unavailable.
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
      screen.getByText(/Pair list unavailable right now/),
    ).toBeInTheDocument();
    expect(
      screen.queryByText(/No CEX sources reporting/),
    ).not.toBeInTheDocument();
    expect(
      screen.queryByText(/No CEX pairs reporting/),
    ).not.toBeInTheDocument();
    // The panel headings must not assert a count either.
    expect(
      screen.queryByText(/0 centralised exchanges/),
    ).not.toBeInTheDocument();
    expect(screen.queryByText(/0 CEX pairs/)).not.toBeInTheDocument();
  });

  it('renders the genuine empty states when the API answers with no rows', async () => {
    vi.mocked(apiGet).mockResolvedValue({ data: [] });
    renderView();
    await waitFor(() =>
      expect(screen.getByText(/No CEX sources reporting/)).toBeInTheDocument(),
    );
    // The pair query waits on the registry's venue list, so it settles later.
    expect(
      await screen.findByText(/No CEX pairs reporting/),
    ).toBeInTheDocument();
    expect(screen.queryByText(/unavailable right now/)).not.toBeInTheDocument();
    expect(screen.getByText(/0 centralised exchanges/)).toBeInTheDocument();
  });

  it('fetches pairs for every registered CEX, not a fixed venue list', async () => {
    const cex = ['binance', 'coinbase', 'kraken', 'bitstamp', 'okx'];
    vi.mocked(apiGet).mockReset();
    vi.mocked(apiGet).mockImplementation(async (path, params) => {
      if (path === '/v1/sources') {
        return {
          data: [
            ...cex.map((name) => ({
              name,
              class: 'exchange',
              subclass: 'cex',
            })),
            { name: 'soroswap', class: 'exchange', subclass: 'amm' },
          ],
        };
      }
      const source = (params as { source?: string } | undefined)?.source;
      return {
        data: [
          {
            base: `crypto:${source?.toUpperCase()}COIN`,
            quote: 'fiat:USD',
            trade_count_24h: 1,
          },
        ],
      };
    });
    renderView();
    await waitFor(() =>
      expect(screen.getByText(/5 CEX pairs/)).toBeInTheDocument(),
    );
    expect(screen.getByText(/OKXCOIN/)).toBeInTheDocument();
    const marketSources = vi
      .mocked(apiGet)
      .mock.calls.filter(([p]) => p === '/v1/markets')
      .map(([, q]) => (q as { source: string }).source)
      .sort();
    expect(marketSources).toEqual([...cex].sort());
  });
  it('orders and formats volumes above 2^53 from the exact decimal', async () => {
    vi.mocked(apiGet).mockReset();
    vi.mocked(apiGet).mockImplementation(async (path, params) => {
      if (path === '/v1/sources') {
        return {
          data: [
            {
              name: 'binance',
              class: 'exchange',
              subclass: 'cex',
              volume_24h_usd: '9007199254740992',
            },
            {
              name: 'kraken',
              class: 'exchange',
              subclass: 'cex',
              volume_24h_usd: '9007199254740993',
            },
            {
              name: 'coinbase',
              class: 'exchange',
              subclass: 'cex',
              volume_24h_usd: '1000000004999999999',
            },
          ],
        };
      }
      const source = (params as { source?: string } | undefined)?.source;
      const vol: Record<string, string> = {
        binance: '9007199254740992',
        kraken: '9007199254740993',
      };
      return {
        data: vol[source ?? '']
          ? [
              {
                base: `crypto:${source}`,
                quote: 'fiat:USD',
                volume_24h_usd: vol[source ?? ''],
                trade_count_24h: 1,
              },
            ]
          : [],
      };
    });
    renderView();
    await screen.findAllByText('$1,000,000T');
    const order = (await screen.findAllByRole('row'))
      .map((r) => /Kraken|Binance/.exec(r.textContent ?? '')?.[0])
      .filter(Boolean);
    expect(order.slice(0, 2)).toEqual(['Kraken', 'Binance']);
  });
});
