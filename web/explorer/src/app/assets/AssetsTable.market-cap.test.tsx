import { describe, it, expect, vi } from 'vitest';
import { fireEvent, render, screen } from '@testing-library/react';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';

import { AssetsTable } from './AssetsTable';
import type { Coin } from '@/api/hooks';

const ISSUER = 'GBLTXF46JTCGMWFJASQLVXMMA36IPYTDCN4EN73HRXCGDCGYBZM3A6VD';

function coin(code: string, fields: Partial<Coin>): Coin {
  return {
    kind: 'stellar_asset',
    asset_id: `${code}-${ISSUER}`,
    code,
    slug: `${code}-${ISSUER}`,
    issuer: ISSUER,
    decimals: 7,
    sep1_status: 'not_applicable',
    first_seen_ledger: 1,
    last_seen_ledger: 1,
    observation_count: 1,
    ...fields,
  } as unknown as Coin;
}

const assets: Coin[] = [
  // Multi-venue, $800 of 24h volume: the server published this cap.
  coin('MULTI', {
    price_usd: '1.00',
    volume_24h_usd: '800.00',
    market_cap_usd: '800000.00',
  }),
  // The server withheld this one as dust liquidity.
  coin('DUST', {
    price_usd: '0.50',
    volume_24h_usd: '5.00',
    market_cap_usd: null,
    market_cap_low_liquidity: true,
  }),
  // Published cap, volume absent from the row.
  coin('NOVOL', {
    price_usd: '2.00',
    market_cap_usd: '2000000.00',
  }),
  // 5,000 whole units at 18 decimals: the largest RAW integer on the page.
  coin('WIDE', {
    decimals: 18,
    circulating_supply: '5000000000000000000000',
  }),
  // 10,000 whole units at 7 decimals: the largest supply actually shown.
  coin('NARROW', { decimals: 7, circulating_supply: '100000000000' }),
];

vi.mock('next/navigation', () => ({
  useRouter: () => ({ push: vi.fn() }),
  useSearchParams: () => new URLSearchParams(''),
}));

vi.mock('@/api/hooks', async () => {
  const actual =
    await vi.importActual<typeof import('@/api/hooks')>('@/api/hooks');
  return {
    ...actual,
    useAssets: () => ({
      data: { assets, next_cursor: '' },
      isLoading: false,
      isError: false,
      error: null,
    }),
  };
});

function renderTable() {
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  });
  return render(
    <QueryClientProvider client={client}>
      <AssetsTable />
    </QueryClientProvider>,
  );
}

// #1013: the table re-derived the dust-liquidity gate with its own $1,000
// volume rule, blanking caps the server deliberately published and
// suppressing any row whose volume was absent.
describe('AssetsTable market cap follows the server verdict', () => {
  it('renders a cap the server published, whatever the row volume', () => {
    renderTable();
    expect(screen.getByText('$800K')).toBeInTheDocument();
    expect(screen.getByText('$2M')).toBeInTheDocument();
  });

  it('explains a cap the server withheld as low liquidity', () => {
    renderTable();
    expect(screen.getByTitle(/negligible liquidity/)).toBeInTheDocument();
  });
});

// The Circulating sort ranked the raw smallest-unit integers while the cell
// shows them scaled by each row's own decimals.
describe('AssetsTable circulating sort follows the displayed units', () => {
  it('ranks by whole units, not raw base units', () => {
    renderTable();
    fireEvent.click(screen.getByRole('button', { name: /Circulating/ }));
    const codes = screen
      .getAllByRole('row')
      .slice(1)
      .map((tr) => tr.querySelectorAll('td')[1]?.textContent ?? '');
    const wide = codes.findIndex((t) => t.startsWith('WIDE'));
    const narrow = codes.findIndex((t) => t.startsWith('NARROW'));
    expect(narrow).toBeGreaterThanOrEqual(0);
    expect(narrow).toBeLessThan(wide);
  });
});
