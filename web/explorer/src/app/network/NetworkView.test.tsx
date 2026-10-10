import { describe, it, expect, vi, beforeEach } from 'vitest';
import { render, screen, waitFor, within } from '@testing-library/react';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';

vi.mock('@/api/client', async () => {
  const actual =
    await vi.importActual<typeof import('@/api/client')>('@/api/client');
  return { ...actual, apiGet: vi.fn() };
});

// Chart canvases are next/dynamic + lightweight-charts; the figures under test are DOM.
vi.mock('next/dynamic', () => ({
  default: () => {
    const Stub = () => <div data-testid="chart-stub" />;
    return Stub;
  },
}));

// The network id and NetworkView's mainnet gate resolve at module load, so each
// test sets the id, resets the module graph, and re-imports the view.
const net = vi.hoisted(() => ({ id: 'mainnet' as 'mainnet' | 'testnet' }));
vi.mock('@/lib/networks', async () => {
  const actual =
    await vi.importActual<typeof import('@/lib/networks')>('@/lib/networks');
  const pick = () => actual.NETWORKS.find((n) => n.id === net.id)!;
  return {
    ...actual,
    get CURRENT_NETWORK() {
      return pick();
    },
    get CURRENT_NETWORK_ID() {
      return net.id;
    },
  };
});

import { apiGet } from '@/api/client';

// Measured live.
const TIP = {
  sequence: 60_000_000,
  close_time: new Date().toISOString(),
  protocol_version: 23,
  base_fee: '100',
  total_coins: '1054439020873472865', // ~105.4B XLM, counts the 2019 burn
  fee_pool: '104692050458598',
  tx_count: 1,
  op_count: 1,
};
const NATIVE = {
  total_supply: '500018068120000000', // 50.0B — the post-2019 constant
  circulating_supply: '346943618543646436', // 34.7B
  supply_basis: 'xlm_sdf_reserve_exclusion',
};

function routeApi(native: unknown, tip: typeof TIP = TIP) {
  vi.mocked(apiGet).mockImplementation(async (path: string) => {
    if (path === '/v1/assets/native') return { data: native };
    if (path === '/v1/ledgers') return { data: { ledgers: [tip] } };
    if (path === '/v1/network/stats')
      return { data: { latest_ledger: TIP.sequence, assets_indexed: 10 } };
    if (path === '/v1/network/throughput') return { data: { buckets: [] } };
    if (path === '/v1/pools') return { data: [] };
    if (path === '/v1/sources') return { data: [] };
    if (path === '/v1/operations') return { data: { operations: [] } };
    return { data: {} };
  });
}

async function renderView() {
  vi.resetModules();
  const { NetworkView } = await import('./NetworkView');
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  });
  return render(
    <QueryClientProvider client={client}>
      <NetworkView />
    </QueryClientProvider>,
  );
}

// The ledger header's total_coins (~105B) still counts the ~55B burned in 2019;
// /v1/assets/native serves 50.0B. The strip must lead with the served figure
// and caption the ledger-header one.
const heroCell = async (label: string, selector?: string) =>
  (await screen.findByText(label, { selector })).closest(
    'div.min-w-0',
  ) as HTMLElement;

describe('NetworkView hero strip — XLM supply', () => {
  beforeEach(() => {
    net.id = 'mainnet';
    vi.mocked(apiGet).mockReset();
  });

  it('on mainnet leads with the served 50.0B total + circulating and captions the ledger total_coins', async () => {
    routeApi(NATIVE);
    await renderView();
    const cell = await heroCell('Total XLM');
    await waitFor(() => expect(cell).toHaveTextContent('50B'));
    expect(cell).toHaveTextContent(
      /34\.69B circulating · SDF reserves excluded/,
    );
    // The ledger-header figure is named and explained, never the headline.
    expect(cell).toHaveTextContent(
      /ledger total_coins 105\.44B · includes the 2019 burn/,
    );
    expect(cell).toHaveTextContent(/10\.47M fee pool/);
    expect(cell.querySelector('.text-2xl')).toHaveTextContent('50B');
    expect(cell.querySelector('.text-2xl')).not.toHaveTextContent('105.44B');
    expect(apiGet).toHaveBeenCalledWith('/v1/assets/native', expect.anything());
  });

  it('on mainnet without served supply falls back to a captioned ledger total_coins', async () => {
    routeApi(null);
    await renderView();
    const cell = await heroCell('Ledger total_coins', 'span');
    await waitFor(() => expect(cell).toHaveTextContent('105.44B'));
    expect(cell).toHaveTextContent(/ledger header · includes the 2019 burn/);
    expect(cell).toHaveTextContent(/10\.47M XLM in fee pool/);
    expect(screen.queryByText('Total XLM')).not.toBeInTheDocument();
  });

  it('on a test net shows the ledger total_coins alone, without mainnet burn claims or a supply fetch', async () => {
    net.id = 'testnet';
    routeApi(NATIVE);
    await renderView();
    const cell = await heroCell('Ledger total_coins', 'span');
    await waitFor(() => expect(cell).toHaveTextContent('105.44B'));
    expect(cell).toHaveTextContent(/ledger header/);
    expect(cell).not.toHaveTextContent(/2019/);
    expect(screen.queryByText('Total XLM')).not.toBeInTheDocument();
    expect(document.body.textContent).not.toMatch(/2019/);
    expect(apiGet).not.toHaveBeenCalledWith(
      '/v1/assets/native',
      expect.anything(),
    );
  });

  it('keeps the fraction of a sub-1,000 XLM fee pool', async () => {
    net.id = 'testnet';
    routeApi(NATIVE, { ...TIP, fee_pool: '55000000' });
    await renderView();
    expect(
      await screen.findByText(/^5\.5 XLM in fee pool$/),
    ).toBeInTheDocument();
  });
});

// The P24 upgrade credited the fee pool with 31,879,035 stroops no transaction
// paid; the API flags it on that day.
describe('dailyFeeBurn', () => {
  it('subtracts the served fee_pool_adjustment across the P24 upgrade day', async () => {
    const { dailyFeeBurn } = await import('./NetworkView');
    const base = 48_000_000_000_000_000n;
    const burned = 5_000_000_000n;
    const bucket = (day: string, pool: bigint, adj?: string) => ({
      day,
      ledgers: 1,
      txs: 1,
      ops: 1,
      events: 1,
      fee_pool: pool.toString(),
      total_coins: '1054439020873472922',
      protocol_version: 24,
      fee_pool_adjustment: adj,
    });
    const rows = [
      bucket('2025-10-21', base),
      bucket('2025-10-22', base + burned + 31_879_035n, '31879035'),
      bucket('2025-10-23', base + 2n * burned + 31_879_035n),
    ];
    expect(dailyFeeBurn(rows)).toEqual([
      { day: '2025-10-22', xlm: 500 },
      { day: '2025-10-23', xlm: 500 },
    ]);
  });
});

describe('NetworkView latest-ledger tile', () => {
  beforeEach(() => {
    net.id = 'mainnet';
    vi.mocked(apiGet).mockReset();
  });

  it('draws the seconds between the served ledger closes', async () => {
    routeApi(NATIVE);
    const base = vi.mocked(apiGet).getMockImplementation()!;
    const t = Date.parse(TIP.close_time);
    const at = (back: number, ms: number) => ({
      ...TIP,
      sequence: TIP.sequence - back,
      close_time: new Date(t - ms).toISOString(),
    });
    vi.mocked(apiGet).mockImplementation(async (path: string, ...rest) => {
      if (path === '/v1/ledgers')
        return { data: { ledgers: [at(0, 0), at(1, 6000), at(2, 11000)] } };
      return base(path, ...rest);
    });
    await renderView();
    const cell = (await heroCell('Latest ledger')).parentElement!;
    expect(
      await within(cell).findByLabelText(
        /Seconds between ledger closes: 2 closes, min 5, max 6/,
      ),
    ).toBeTruthy();
  });
});

describe('NetworkView active sources volume', () => {
  beforeEach(() => {
    net.id = 'mainnet';
    vi.mocked(apiGet).mockReset();
  });

  it('ranks and rounds source volume above 2^53 from the exact decimal', async () => {
    routeApi(NATIVE);
    const base = vi.mocked(apiGet).getMockImplementation()!;
    const source = (name: string, volume_24h_usd: string) => ({
      name,
      class: 'exchange',
      subclass: 'amm',
      volume_24h_usd,
    });
    vi.mocked(apiGet).mockImplementation(async (path: string, ...rest) => {
      if (path === '/v1/sources') {
        return {
          data: [
            source('aquarius', '9007199254740992'),
            source('soroswap', '9007199254740993'),
            source('phoenix', '1000000004999999999'),
          ],
        };
      }
      return base(path, ...rest);
    });
    await renderView();
    const heading = await screen.findByText('Most active Stellar sources');
    const panel = heading.closest('section') as HTMLElement;
    await waitFor(() => expect(panel).toHaveTextContent('soroswap'));
    expect(panel).toHaveTextContent('$1,000,000T');
    const text = panel.textContent ?? '';
    expect(text.indexOf('soroswap')).toBeLessThan(text.indexOf('aquarius'));
  });
});

describe('throughputSummary', () => {
  it('returns the total, the per-second mean and the busiest day', async () => {
    const { throughputSummary } = await import('./NetworkView');
    const got = throughputSummary(
      [
        { day: '2026-10-01', ops: 864_000 },
        { day: '2026-10-02', ops: 1_728_000 },
      ] as Parameters<typeof throughputSummary>[0],
      'ops',
    );
    expect(got.total).toBe(2_592_000);
    expect(got.perSecond).toBe(15);
    expect(got.peak).toEqual({ day: '2026-10-02', value: 1_728_000 });
  });

  it('has no peak for an empty window', async () => {
    const { throughputSummary } = await import('./NetworkView');
    expect(throughputSummary([], 'txs')).toEqual({
      total: 0,
      perSecond: 0,
      peak: null,
    });
  });
});

describe('closeGaps', () => {
  it('gives each ledger its seconds after the previous and none to the oldest', async () => {
    const { closeGaps } = await import('./NetworkView');
    expect(
      closeGaps([
        { close_time: '2026-10-10T00:00:16Z' },
        { close_time: '2026-10-10T00:00:05Z' },
        { close_time: '2026-10-10T00:00:00Z' },
      ]),
    ).toEqual([11, 5, null]);
  });
});
