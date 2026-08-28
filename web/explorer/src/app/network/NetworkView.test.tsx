import { describe, it, expect, vi, beforeEach } from 'vitest';
import { render, screen, waitFor } from '@testing-library/react';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';

vi.mock('@/api/client', async () => {
  const actual =
    await vi.importActual<typeof import('@/api/client')>('@/api/client');
  return { ...actual, apiGet: vi.fn() };
});

// The chart canvases are next/dynamic + lightweight-charts; the semantics
// under test (the hero-strip figures and their captions) are DOM.
vi.mock('next/dynamic', () => ({
  default: () => {
    const Stub = () => <div data-testid="chart-stub" />;
    return Stub;
  },
}));

// CURRENT_NETWORK is resolved from NEXT_PUBLIC_NETWORK at module load and
// NetworkView derives its mainnet gate at module load too, so each test
// sets the id, resets the module graph, and re-imports the view.
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

// Measured live 2026-08-28.
const TIP = {
  sequence: 60_000_000,
  close_time: new Date().toISOString(),
  protocol_version: 23,
  base_fee: 100,
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

function routeApi(native: unknown) {
  vi.mocked(apiGet).mockImplementation(async (path: string) => {
    if (path === '/v1/assets/native') return { data: native };
    if (path === '/v1/ledgers') return { data: { ledgers: [TIP] } };
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

// Audit 2026-08-28 ("XLM supply 2.11× route divergence"): the network
// strip showed the ledger header's total_coins (~105B, which still counts
// the ~55B burned in 2019) as an unlabeled "Total XLM" while
// /v1/assets/native serves total_supply 50.0B. Both are right under their
// definitions — the strip must lead with the served figure and caption the
// ledger-header one with what it is.
describe('NetworkView hero strip — XLM supply', () => {
  beforeEach(() => {
    net.id = 'mainnet';
    vi.mocked(apiGet).mockReset();
  });

  it('on mainnet leads with the served 50.0B total + circulating and captions the ledger total_coins', async () => {
    routeApi(NATIVE);
    await renderView();
    const totalLabel = await screen.findByText('Total XLM');
    const cell = totalLabel.closest('div.min-w-0') as HTMLElement;
    await waitFor(() => expect(cell).toHaveTextContent('50B'));
    expect(cell).toHaveTextContent(
      /34\.69B circulating · SDF reserves excluded/,
    );
    // The ledger-header figure is still there, but named and explained —
    // never the headline value.
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
    const label = await screen.findByText('Ledger total_coins', {
      selector: 'span',
    });
    const cell = label.closest('div.min-w-0') as HTMLElement;
    await waitFor(() => expect(cell).toHaveTextContent('105.44B'));
    expect(cell).toHaveTextContent(/ledger header · includes the 2019 burn/);
    expect(cell).toHaveTextContent(/10\.47M XLM in fee pool/);
    expect(screen.queryByText('Total XLM')).not.toBeInTheDocument();
  });

  it('on a test net shows the ledger total_coins alone, without mainnet burn claims or a supply fetch', async () => {
    net.id = 'testnet';
    routeApi(NATIVE);
    await renderView();
    const label = await screen.findByText('Ledger total_coins', {
      selector: 'span',
    });
    const cell = label.closest('div.min-w-0') as HTMLElement;
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
});
