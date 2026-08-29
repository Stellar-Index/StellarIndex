// #328: the /assets directory renders six aggregator-derived columns —
// price, 1h/24h/7d change, market cap, 24h volume — plus a 7d price
// sparkline. On a network with no aggregator every one of those values is
// null, so a test-net visitor got a directory whose right-hand two-thirds
// was a wall of em-dashes, and the table asked the API for a 7d price
// series (`include=sparkline7d`) that cannot exist there.
//
// CURRENT_NETWORK is a module-load build constant, so each case re-imports
// the component with a fresh registry — same convention as
// networks.test.ts. The @/api/hooks stub captures the include options the
// component asked for, so the fetch-shape half is pinned too.
import { afterEach, describe, expect, it, vi } from 'vitest';
import { render, screen } from '@testing-library/react';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';

import type { Coin } from '@/api/hooks';

// One priced row and one unpriced row. On mainnet both render every
// column; on a lean net the priced columns must not exist at all.
const coins = [
  {
    asset_id: 'native',
    slug: 'xlm',
    code: 'XLM',
    name: 'Stellar Lumens',
    class: 'crypto',
    decimals: 7,
    price_usd: '0.4123',
    change_1h_pct: '0.5',
    change_24h_pct: '1.25',
    change_7d_pct: '-2.5',
    market_cap_usd: '12000000000',
    volume_24h_usd: '4500000',
    circulating_supply: '300000000000000',
    observation_count: 4242,
  },
] as unknown as Coin[];

let lastOptions: Record<string, unknown> | undefined;

vi.mock('@/api/hooks', async () => {
  const actual =
    await vi.importActual<typeof import('@/api/hooks')>('@/api/hooks');
  return {
    ...actual,
    useAssets: (
      _class: unknown,
      _limit: unknown,
      _cursor: unknown,
      _q: unknown,
      options: Record<string, unknown>,
    ) => {
      lastOptions = options;
      return {
        data: { assets: coins, next_cursor: '' },
        isLoading: false,
        isError: false,
        error: null,
      };
    },
  };
});

async function renderFor(network: string) {
  vi.resetModules();
  vi.stubEnv('NEXT_PUBLIC_NETWORK', network);
  const { AssetsTable } = await import('./AssetsTable');
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  });
  return render(
    <QueryClientProvider client={client}>
      <AssetsTable />
    </QueryClientProvider>,
  );
}

const PRICED_COLUMNS = [
  'Price',
  '1h %',
  '24h %',
  '7d %',
  'Market cap',
  'Volume 24h',
  '7d chart',
];
const NATIVE_COLUMNS = ['Asset', 'Class', 'Circulating'];

afterEach(() => {
  vi.unstubAllEnvs();
  vi.resetModules();
  lastOptions = undefined;
});

describe('AssetsTable per-network columns', () => {
  it('renders the full priced directory on mainnet', async () => {
    const { container } = await renderFor('mainnet');
    for (const label of [...PRICED_COLUMNS, ...NATIVE_COLUMNS]) {
      expect(screen.getByText(label)).toBeInTheDocument();
    }
    // The priced VALUES, not just the headers (formatPriceSmall renders a
    // sub-$1 price at 6dp; formatCompact abbreviates the cap/volume).
    expect(container.textContent).toContain('$0.412300');
    expect(container.textContent).toContain('$12B');
    expect(container.textContent).toContain('$4.5M');
    expect(lastOptions).toEqual({ sparkline7d: true });
  });

  it.each(['testnet', 'futurenet'])(
    'drops every USD column on %s and stops asking for the price series',
    async (network) => {
      const { container } = await renderFor(network);
      for (const label of PRICED_COLUMNS) {
        expect(screen.queryByText(label)).toBeNull();
      }
      // Not merely blanked to "—" — the columns are gone, so a USD figure
      // can never be rendered on a net that computes none.
      expect(container.textContent).not.toContain('$0.412300');
      expect(container.textContent).not.toContain('$12B');
      expect(container.textContent).not.toContain('$4.5M');
      // The chain-native columns stay — this is a gate, not a blackout.
      for (const label of NATIVE_COLUMNS) {
        expect(screen.getByText(label)).toBeInTheDocument();
      }
      expect(screen.getByText('XLM')).toBeInTheDocument();
      expect(lastOptions).toEqual({ sparkline7d: false });
    },
  );
});
