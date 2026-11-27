import { describe, it, expect, vi, beforeEach } from 'vitest';
import { render, screen, waitFor, within } from '@testing-library/react';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import type { ReactNode } from 'react';

import { routeFor } from '@/lib/route-graph';

vi.mock('@/api/client', async () => {
  const actual =
    await vi.importActual<typeof import('@/api/client')>('@/api/client');
  return { ...actual, apiGet: vi.fn() };
});
vi.mock('./LendingOverviewCharts', () => ({
  LendingOverviewCharts: () => <div />,
}));

import { apiGet } from '@/api/client';
import LendingPage from './page';
import BlendLendingPage, { metadata as blendMeta } from './blend/page';
import BlntLendingPage, { metadata as blntMeta } from './blnt/page';

// Uncurated contract ids, so each row's protocol comes from the listing.
const BLEND_POOL = `C${'A'.repeat(55)}`;
const BLNT_POOL = `C${'B'.repeat(55)}`;

const protocolRow = (protocol: 'blend' | 'blnt', supplied: string) => ({
  protocol,
  pool_count: 1,
  reserve_count: 2,
  total_supplied_usd: supplied,
  total_borrowed_usd: '10.00',
  available_usd: '5.00',
  lower_bound: false,
  excluded: [],
  token: null,
});

beforeEach(() => {
  vi.mocked(apiGet).mockReset();
  vi.mocked(apiGet).mockImplementation(async (path: string) => {
    if (path === '/v1/lending/pools')
      return {
        data: [
          { protocol: 'blend', pool: BLEND_POOL, auctions_total: 3 },
          { protocol: 'blnt', pool: BLNT_POOL, auctions_total: 4 },
        ],
      };
    return {
      data: [protocolRow('blend', '1000.00'), protocolRow('blnt', '2000.00')],
    };
  });
});

function renderPage(ui: ReactNode) {
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  });
  return render(
    <QueryClientProvider client={client}>{ui}</QueryClientProvider>,
  );
}

const poolHrefs = () =>
  screen
    .getAllByRole('link')
    .map((a) => a.getAttribute('href'))
    .filter(
      (h) => h === `/lending/${BLEND_POOL}` || h === `/lending/${BLNT_POOL}`,
    );

describe('lending protocol pages', () => {
  it('the Blend page lists only Blend pools and Blend totals', async () => {
    renderPage(<BlendLendingPage />);
    await screen.findAllByTitle(BLEND_POOL);
    expect(new Set(poolHrefs())).toEqual(new Set([`/lending/${BLEND_POOL}`]));
    expect(screen.getByRole('heading', { level: 1 })).toHaveTextContent(
      'Blend lending',
    );
    expect(screen.getByTitle('$1,000.00')).toBeInTheDocument();
    expect(screen.queryByTitle('$2,000.00')).toBeNull();
    expect(screen.queryByText('Tiered backstop')).toBeNull();
  });

  it('the BLNT page lists only BLNT pools and BLNT totals', async () => {
    renderPage(<BlntLendingPage />);
    await screen.findAllByTitle(BLNT_POOL);
    expect(new Set(poolHrefs())).toEqual(new Set([`/lending/${BLNT_POOL}`]));
    expect(screen.getByTitle('$2,000.00')).toBeInTheDocument();
    expect(screen.queryByTitle('$1,000.00')).toBeNull();
    expect(screen.queryByText('Reflector-priced')).toBeNull();
  });

  it('sets a canonical, title and description per page', () => {
    expect(blendMeta.alternates?.canonical).toBe('/lending/blend');
    expect(blntMeta.alternates?.canonical).toBe('/lending/blnt');
    expect(blendMeta.title).toMatch(/^Blend lending/);
    expect(blntMeta.title).toMatch(/^BLNT V3 lending/);
    expect(blendMeta.description).toBeTruthy();
    expect(blntMeta.description).toBeTruthy();
  });

  it('resolves both paths to their static pages, not the pool page', () => {
    expect(routeFor('/lending/blend/')).toBe('/lending/blend');
    expect(routeFor('/lending/blnt/')).toBe('/lending/blnt');
    expect(routeFor(`/lending/${BLEND_POOL}/`)).toBe('/lending/[pool]');
  });
});

describe('lending landing', () => {
  it('links one card per protocol and keeps both protocols in the pools table', async () => {
    renderPage(<LendingPage />);
    await screen.findAllByTitle(BLNT_POOL);
    for (const [name, href] of [
      ['Blend', '/lending/blend'],
      ['BLNT V3', '/lending/blnt'],
    ]) {
      expect(
        screen.getByRole('link', {
          name: `${name} revenue, token and pools`,
        }),
      ).toHaveAttribute('href', href);
    }
    expect(new Set(poolHrefs())).toEqual(
      new Set([`/lending/${BLEND_POOL}`, `/lending/${BLNT_POOL}`]),
    );
    await waitFor(() =>
      expect(screen.getByTitle('$2,000.00')).toBeInTheDocument(),
    );
    // The cards carry three figures; the token figures live on the protocol pages.
    expect(screen.queryByText('Market cap')).toBeNull();
    expect(screen.queryByText('Interest & revenue')).toBeNull();
    const nav = screen.getByRole('navigation', { name: 'Lending sections' });
    expect(within(nav).getByRole('link')).toHaveAttribute(
      'href',
      '/lending/rates',
    );
  });
});
