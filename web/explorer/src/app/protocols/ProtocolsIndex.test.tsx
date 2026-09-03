import { describe, it, expect, vi, beforeEach } from 'vitest';
import { render, screen, waitFor } from '@testing-library/react';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';

vi.mock('@/api/client', async () => {
  const actual = await vi.importActual<typeof import('@/api/client')>('@/api/client');
  return { ...actual, apiGet: vi.fn() };
});

import { apiGet } from '@/api/client';
import { ProtocolsIndex } from './ProtocolsIndex';

// #338 — /v1/protocols carries a headline `tvl_total` alongside
// `protocols[]`, and this component's query used to `return
// env.data?.protocols ?? []`, dropping the money figure on the floor.
// These tests pin that the total reaches the page, and that its ABSENT
// case (tvl_total is `omitempty`; omitted when the reconciliation could
// admit nothing) renders as absence rather than as a zero.

const PROTOCOLS = [
  {
    name: 'aquarius',
    category: 'amm',
    description: 'Aquarius AMM',
    genesis_ledger: 1,
    factories: [],
    contract_count: 228,
    tvl: {
      tvl_usd: '39342115.63',
      pools_total: 228,
      pools_priced: 51,
      unpriced_pools: 177,
      basis: 'sum of each pool latest post-state reserve snapshot',
    },
  },
  {
    name: 'comet',
    category: 'amm',
    description: 'Comet pool',
    genesis_ledger: 1,
    factories: [],
    contract_count: 1,
    tvl: {
      tvl_usd: '1569.77',
      pools_total: 1,
      pools_priced: 1,
      unpriced_pools: 0,
      basis: 'sum of current per-token pool balance records',
    },
  },
];

const TVL_TOTAL = {
  tvl_usd: '39343685.40',
  protocols: ['aquarius', 'comet'],
  lower_bound: true,
  pools_total: 229,
  pools_priced: 52,
  unpriced_pools: 177,
  as_of_ledger: 63_000_050,
  as_of: '2026-09-03T04:30:32Z',
  basis: 'exact sum of the published aquarius and comet figures',
  excluded: [
    { subject: 'classic liquidity pools', reason: 'CAP-38 pools are not valued yet' },
  ],
};

function mockProtocols(tvlTotal?: unknown) {
  vi.mocked(apiGet).mockImplementation(async (path: string) => {
    if (path === '/v1/protocols') {
      return {
        data: { protocols: PROTOCOLS, ...(tvlTotal ? { tvl_total: tvlTotal } : {}) },
      } as never;
    }
    return { data: [] } as never;
  });
}

function renderIndex() {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(
    <QueryClientProvider client={client}>
      <ProtocolsIndex />
    </QueryClientProvider>,
  );
}

describe('ProtocolsIndex headline TVL total', () => {
  beforeEach(() => vi.mocked(apiGet).mockReset());

  it('renders the served tvl_total exactly, with its provenance', async () => {
    mockProtocols(TVL_TOTAL);
    renderIndex();
    await waitFor(() =>
      expect(screen.getByText('Total value locked')).toBeInTheDocument(),
    );
    // The exact decimal string, grouped — not a compacted "$39.34M", and
    // not a Number()-parsed approximation of it.
    expect(screen.getByText(/\$39,343,685\.40/)).toBeInTheDocument();
    // lower_bound is true, so the "at least" reading is on the page.
    expect(screen.getByText('≥')).toBeInTheDocument();
    expect(screen.getByText(/52 of 229 pools priced/)).toBeInTheDocument();
    // as_of_ledger + as_of beside the figure; basis and excluded reachable.
    expect(screen.getByText(/ledger 63,000,050/)).toBeInTheDocument();
    expect(
      screen.getByText(/exact sum of the published aquarius and comet figures/),
    ).toBeInTheDocument();
    expect(screen.getByText('What this total excludes')).toBeInTheDocument();
    expect(
      screen.getByText('CAP-38 pools are not valued yet'),
    ).toBeInTheDocument();
  });

  it('renders an omitted tvl_total as absent — never $0.00, never a dash', async () => {
    mockProtocols(undefined);
    renderIndex();
    // The per-protocol bars still arrive…
    await waitFor(() =>
      expect(screen.getByText('Value locked (USD)')).toBeInTheDocument(),
    );
    // …and the headline simply is not there.
    expect(screen.queryByText('Total value locked')).not.toBeInTheDocument();
    expect(screen.queryByText(/\$0\.00/)).not.toBeInTheDocument();
  });
});

// The category landings (/bridges, /yield) are one-liners over this
// component with `lockedCategory` set. The headline stats used to be
// summed over every card in the directory while the grid below showed
// one category, so /bridges published the whole directory's figures —
// measured live 2026-09-03: 16 protocols and 1,146,432 events beside
// the 2 bridges that actually emitted 2,732.

const DIRECTORY = [
  {
    name: 'cctp',
    category: 'bridge',
    description: 'Canonical burn-and-mint USDC bridging.',
    genesis_ledger: 1,
    factories: [],
    contract_count: 4,
    events_24h: 1_700,
    completeness: { complete: true, watermark_ledger: 63_000_000 },
  },
  {
    name: 'rozo',
    category: 'bridge',
    description: 'Intent-bridge payment settlement.',
    genesis_ledger: 1,
    factories: [],
    contract_count: 2,
    events_24h: 1_032,
    completeness: { complete: true, watermark_ledger: 63_000_000 },
  },
  {
    name: 'sdex',
    category: 'dex',
    description: "Stellar's protocol-native order book.",
    genesis_ledger: 1,
    factories: [],
    contract_count: 0,
    events_24h: 1_090_927,
    completeness: { complete: true, watermark_ledger: 63_000_000 },
  },
  {
    name: 'defindex',
    category: 'yield',
    description: 'Yield vaults and strategies.',
    genesis_ledger: 1,
    factories: [],
    contract_count: 9,
    events_24h: 2_468,
    completeness: { complete: false, watermark_ledger: 62_900_000 },
  },
];

function mockDirectory() {
  vi.mocked(apiGet).mockImplementation(async (path: string) => {
    if (path === '/v1/protocols') {
      return { data: { protocols: DIRECTORY } } as never;
    }
    return { data: [] } as never;
  });
}

// Stat renders the label in a span inside its own div; the value is that
// div's next sibling.
function statValue(label: string): string {
  return (
    screen.getByText(label).parentElement?.nextElementSibling?.textContent ?? ''
  );
}

function cardCount(): number {
  return document.querySelectorAll('[data-source] > a').length;
}

describe('ProtocolsIndex headline stats under a locked category', () => {
  beforeEach(() => vi.mocked(apiGet).mockReset());

  it('counts the category shown, not the whole directory', async () => {
    mockDirectory();
    const client = new QueryClient({
      defaultOptions: { queries: { retry: false } },
    });
    render(
      <QueryClientProvider client={client}>
        <ProtocolsIndex lockedCategory="bridge" title="Bridges" />
      </QueryClientProvider>,
    );
    await waitFor(() => expect(cardCount()).toBe(2));

    // The headline is the grid: two bridge cards, two counts.
    expect(statValue('Protocols')).toBe(String(cardCount()));
    expect(statValue('Verified complete')).toBe('2');
    // 1,700 + 1,032 — the directory's 1,096,127 ("1.1M") is not this
    // page's number, and must appear nowhere on it.
    expect(statValue('Events · last 24h')).toBe('2.73K');
    expect(screen.queryByText('1.1M')).not.toBeInTheDocument();
  });

  it('renders the count as absence when the directory is unreachable', async () => {
    // The static-registry fallback has no categories, so a locked page
    // has nothing to list: that is a dead endpoint, not zero bridges.
    vi.mocked(apiGet).mockImplementation(async (path: string) => {
      if (path === '/v1/protocols') throw new Error('HTTP 503');
      return { data: [] } as never;
    });
    const client = new QueryClient({
      defaultOptions: { queries: { retry: false } },
    });
    render(
      <QueryClientProvider client={client}>
        <ProtocolsIndex lockedCategory="bridge" title="Bridges" />
      </QueryClientProvider>,
    );
    await waitFor(() =>
      expect(screen.getByText('Live stats unavailable')).toBeInTheDocument(),
    );
    expect(cardCount()).toBe(0);
    expect(statValue('Protocols')).toBe('—');
  });
});
