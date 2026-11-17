import { describe, it, expect, vi } from 'vitest';
import { render, screen } from '@testing-library/react';
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
  // Trustline-sum supply and a volume with unvalued trades: both are floors.
  coin('FLOOR', {
    price_usd: '1.00',
    volume_24h_usd: '1200.00',
    volume_lower_bound: true,
    market_cap_usd: '5000000.00',
    circulating_supply: '50000000000000',
    supply_basis: 'classic_trustline_sum',
  }),
  // Catalogue rows carry the flag but no supply_basis.
  coin('FLAGGED', {
    price_usd: '1.00',
    market_cap_usd: '3000000.00',
    market_cap_lower_bound: true,
  }),
  coin('DERIVED', {
    price_usd: '1.00',
    market_cap_usd: '2000000.00',
    circulating_supply: '20000000000000',
    supply_basis: 'classic_lake_flows',
  }),
  // A full supply reading and a fully valued volume.
  coin('EXACT', {
    price_usd: '1.00',
    volume_24h_usd: '800.00',
    market_cap_usd: '800000.00',
    circulating_supply: '8000000000000',
    supply_basis: 'issuer_exclusion',
  }),
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

// The server flags a floor supply and a volume that leaves trades out; the
// table printed both as exact figures.
describe('AssetsTable marks the figures the server flagged as floors', () => {
  it('prefixes a floor cap, supply and volume with ≥', () => {
    renderTable();
    expect(screen.getByText('≥ $5M')).toBeInTheDocument();
    expect(screen.getByText('≥ $1.2K')).toBeInTheDocument();
    expect(screen.getByText(/^≥\s+5M$/)).toBeInTheDocument();
  });

  it('honours market_cap_lower_bound without a supply_basis', () => {
    renderTable();
    expect(screen.getByText('≥ $3M')).toBeInTheDocument();
  });

  it('marks a derived lake-flows cap and supply as an upper reading', () => {
    renderTable();
    expect(screen.getByText('≤ $2M')).toBeInTheDocument();
    expect(screen.getByText(/^≤\s+2M$/)).toBeInTheDocument();
  });

  it('leaves exact figures unmarked', () => {
    renderTable();
    expect(screen.getByText('$800K')).toBeInTheDocument();
    expect(screen.getByText('$800')).toBeInTheDocument();
    expect(screen.getByText('800K')).toBeInTheDocument();
  });
});
