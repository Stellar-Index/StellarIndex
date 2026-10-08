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
    type: 'classic',
    sep1_status: 'not_applicable',
    first_seen_ledger: 1,
    last_seen_ledger: 1,
    observation_count: 1,
    ...fields,
  } as unknown as Coin;
}

// `kind` is the constant ADR-0042 wire-shape discriminator ("stellar_asset"
// on every row) and `type` is the protocol class, neither of which is the
// verified-currency taxonomy the "Class" column cell (ClassBadge) renders —
// that's `class`. Sorting by kind/type never reorders the visible column.
// Deliberately NOT already in class order: a no-op sort (the unfixed
// accessor) would leave this order untouched and the assertions below
// would pass vacuously if the fixture happened to already be sorted.
const assets: Coin[] = [
  coin('BBB', { class: 'fiat' }),
  coin('CCC', { class: 'stablecoin' }),
  coin('AAA', { class: 'crypto' }),
  // A decimals-mismatch refusal must not render like an
  // ordinary "no data yet" dash.
  coin('MISMATCH', {
    class: 'crypto',
    price_usd: '1.00',
    market_cap_usd: null,
    market_cap_decimals_mismatch: true,
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

function firstColumnCodes(): string[] {
  return screen
    .getAllByRole('row')
    .slice(1)
    .map((tr) => tr.querySelectorAll('td')[1]?.textContent ?? '');
}

// Clicking "Class" sorted by the constant `kind` field
// (always "stellar_asset"), so the visible order never changed.
describe('AssetsTable "Class" column sort', () => {
  it('orders rows by the verified-currency class the badge renders', () => {
    renderTable();
    fireEvent.click(screen.getByRole('button', { name: /Class/ }));
    const codes = firstColumnCodes();
    // ascending alpha on class: crypto < fiat < stablecoin
    expect(codes.findIndex((t) => t.startsWith('AAA'))).toBeLessThan(
      codes.findIndex((t) => t.startsWith('BBB')),
    );
    expect(codes.findIndex((t) => t.startsWith('BBB'))).toBeLessThan(
      codes.findIndex((t) => t.startsWith('CCC')),
    );
  });
});

// market_cap_decimals_mismatch (the server's REFUSAL because
// its two decimals resolvers disagree) rendered identically to "no data" —
// a self-clearing scale disagreement was indistinguishable from an asset
// that simply has no supply/price yet.
describe('AssetsTable market cap decimals-mismatch verdict', () => {
  it('explains a cap the server refused for a decimals mismatch', () => {
    renderTable();
    expect(
      screen.getByTitle(
        /on-chain decimals\(\) and the price projection disagree/,
      ),
    ).toBeInTheDocument();
  });
});
