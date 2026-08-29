import { describe, it, expect, vi } from 'vitest';
import { render, screen, waitFor, within } from '@testing-library/react';
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

  // Oracle capture-totality (PR-5): the page is THE opt-in for the
  // `raw:<symbol>` rows /v1/oracle/streams otherwise omits. Those rows are
  // reference-only, so they render in their own "Unmapped feeds" section
  // under the raw on-wire symbol, unlinked, and never join the mapped list.
  describe('unmapped raw: rows', () => {
    const mapped = {
      source: 'reflector-cex',
      asset: 'crypto:BTC',
      quote: 'fiat:USD',
      ts: '2026-08-28T12:00:00Z',
      price: '65000.5',
      price_raw: '6500050000000',
      decimals: 14,
      mapped: true,
    };
    const raw = {
      source: 'reflector-cex',
      asset: 'raw:NOTACOIN',
      quote: 'fiat:USD',
      ts: '2026-08-28T12:00:00Z',
      price: '1.25',
      price_raw: '125000000000000',
      decimals: 14,
      mapped: false,
    };

    function mockApi(streams: unknown[]) {
      vi.mocked(apiGet).mockImplementation(async (path: string) => {
        if (path === '/v1/oracle/streams') return { data: streams };
        return { data: [] };
      });
    }

    it('requests /v1/oracle/streams with include_unmapped=true', async () => {
      mockApi([mapped]);
      renderView();
      await waitFor(() => expect(screen.getByText('BTC')).toBeInTheDocument());
      expect(apiGet).toHaveBeenCalledWith('/v1/oracle/streams', { include_unmapped: 'true' });
    });

    it('lists a raw: row only in the Unmapped feeds section, by on-wire symbol, unlinked', async () => {
      mockApi([mapped, raw]);
      renderView();
      await waitFor(() => expect(screen.getByText('BTC')).toBeInTheDocument());

      const mappedPanel = screen.getByText(/^Price streams/).closest('section') as HTMLElement;
      const unmappedPanel = screen.getByText(/^Unmapped feeds/).closest('section') as HTMLElement;
      expect(mappedPanel).not.toBe(unmappedPanel);

      // Mapped table: exactly one mapped row; no raw symbol in it.
      expect(within(mappedPanel).getByText('BTC')).toBeInTheDocument();
      expect(within(mappedPanel).queryByText(/NOTACOIN/)).not.toBeInTheDocument();
      expect(screen.getByText('Price streams (1 active)')).toBeInTheDocument();

      // Unmapped section: the raw on-wire symbol, verbatim, not linked
      // anywhere under /assets/.
      expect(screen.getByText('Unmapped feeds (1)')).toBeInTheDocument();
      const sym = within(unmappedPanel).getByText('NOTACOIN');
      expect(sym.closest('a')).toBeNull();
      expect(sym).toHaveAttribute('title', expect.stringContaining('raw:NOTACOIN'));
      const assetHrefs = Array.from(document.querySelectorAll('a[href^="/assets/"]')).map((a) =>
        a.getAttribute('href'),
      );
      expect(assetHrefs.some((h) => h?.toLowerCase().includes('raw'))).toBe(false);
    });

    it('renders the genuine empty state for unmapped feeds when the API answers with none', async () => {
      mockApi([mapped]);
      renderView();
      await waitFor(() => expect(screen.getByText('BTC')).toBeInTheDocument());
      expect(screen.getByText(/No unmapped feeds in the last 7 days/)).toBeInTheDocument();
    });
  });
});
