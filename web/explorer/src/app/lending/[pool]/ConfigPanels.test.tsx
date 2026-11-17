import { describe, it, expect, vi, beforeEach } from 'vitest';
import { fireEvent, render, screen } from '@testing-library/react';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';

import { ConfigHistory } from './ConfigHistory';
import { PoolDetails } from './PoolDetails';
import { PoolReserves } from './PoolReserves';

vi.mock('@/api/client', async () => {
  const actual =
    await vi.importActual<typeof import('@/api/client')>('@/api/client');
  return { ...actual, apiGet: vi.fn() };
});
vi.mock('@/lib/live/hooks', async (importOriginal) => ({
  ...(await importOriginal<typeof import('@/lib/live/hooks')>()),
  useLedgerFollow: () => {},
}));

import { apiGet } from '@/api/client';

const POOL = 'CAJJZSGMMM3PD7N33TAPHGBUGTB43OC73HVIK2L2G6BNGGGYOSSYBXBD';
const ORACLE = 'CBORACLEAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA';

function wrap(ui: React.ReactNode) {
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  });
  return render(
    <QueryClientProvider client={client}>{ui}</QueryClientProvider>,
  );
}

const cfg = {
  index: 0,
  decimals: 7,
  collateral_factor: '9500000',
  liability_factor: '8000000',
  target_utilization: '7500000',
  max_utilization: '9500000',
  r_base: '100000',
  r_one: '500000',
  r_two: '5000000',
  r_three: '15000000',
  reactivity: '20',
  supply_cap: null,
  enabled: true,
};

const reserves = {
  data: {
    pool: POOL,
    tvl_usd: '1000',
    config_decimals: 7,
    pool_config: {
      oracle: ORACLE,
      admin: 'GADMIN',
      backstop_take_rate: '1000000',
      max_positions: 4,
      status: 7,
    },
    reserves: [
      {
        asset: 'native',
        decimals: 7,
        supplied: '10000000000',
        borrowed: '5000000000',
        supplied_usd: '1000',
        borrowed_usd: '500',
        utilization_pct: 50,
        borrow_apr: 0.01,
        supply_apr: 0.005,
        price_usd: '0.12',
        price_stale: true,
        config: cfg,
        ir_mod: '10000000',
        ir_mod_decimals: 7,
      },
    ],
  },
};

beforeEach(() => vi.mocked(apiGet).mockReset());

describe('PoolDetails', () => {
  it('labels a raw min collateral as base units', async () => {
    vi.mocked(apiGet).mockResolvedValue({
      data: {
        ...reserves.data,
        pool_config: { ...reserves.data.pool_config, min_collateral: '12345' },
      },
    });
    wrap(<PoolDetails pool={POOL} />);
    expect(await screen.findByText('12345 base units')).toBeInTheDocument();
  });

  it('shows the raw status code when there is no label, and a null min collateral as a dash', async () => {
    vi.mocked(apiGet).mockResolvedValue(reserves);
    wrap(<PoolDetails pool={POOL} />);
    expect(await screen.findByText('Status code 7')).toBeInTheDocument();
    expect(screen.getByText('10%')).toBeInTheDocument();
    expect(screen.getByText(ORACLE)).toHaveAttribute(
      'href',
      `/contracts/${ORACLE}/`,
    );
    expect(screen.getByText('GADMIN')).toHaveAttribute(
      'href',
      '/accounts/GADMIN/',
    );
  });
});

describe('PoolReserves config columns', () => {
  it('shows factors, a stale price and the rate model on expand', async () => {
    vi.mocked(apiGet).mockResolvedValue(reserves);
    wrap(<PoolReserves pool={POOL} />);
    expect((await screen.findAllByText('95%')).length).toBeGreaterThan(0);
    expect(screen.getByText('stale')).toBeInTheDocument();
    fireEvent.click(screen.getByRole('button', { name: 'Details' }));
    expect(screen.getByText('Interest-rate model')).toBeInTheDocument();
    expect(
      screen.getByRole('img', { name: /Borrow APR from 0%/ }),
    ).toBeInTheDocument();
  });

  it('shows dashes when the config is null', async () => {
    const r = structuredClone(reserves);
    r.data.reserves[0].config = null as never;
    r.data.reserves[0].price_usd = null as never;
    vi.mocked(apiGet).mockResolvedValue(r);
    wrap(<PoolReserves pool={POOL} />);
    await screen.findByText('Reserve composition');
    expect(screen.queryByText('stale')).toBeNull();
    fireEvent.click(await screen.findByRole('button', { name: 'Details' }));
    expect(screen.getByText(/rate curve is unavailable/)).toBeInTheDocument();
  });
});

describe('ConfigHistory', () => {
  it('lists changes newest first and loads more through the cursor', async () => {
    vi.mocked(apiGet)
      .mockResolvedValueOnce({
        data: {
          pool: POOL,
          config_decimals: 7,
          changes: [
            {
              ledger: 200,
              close_time: '2026-01-02T00:00:00Z',
              tx_hash: 'b'.repeat(64),
              kind: 'set_reserve',
              asset: null,
              config: cfg,
            },
          ],
        },
        pagination: { next: 'cur1' },
      })
      .mockResolvedValueOnce({
        data: {
          pool: POOL,
          config_decimals: 7,
          changes: [
            {
              ledger: 100,
              close_time: '2026-01-01T00:00:00Z',
              tx_hash: 'a'.repeat(64),
              kind: 'set_status',
              asset: null,
              pool_config: { status: 3, status_label: 'on_ice' },
            },
          ],
        },
      });
    wrap(<ConfigHistory pool={POOL} />);
    expect(await screen.findByText('Reserve applied')).toBeInTheDocument();
    fireEvent.click(screen.getByRole('button', { name: 'Load more' }));
    expect(await screen.findByText('Status changed')).toBeInTheDocument();
    expect(apiGet).toHaveBeenLastCalledWith(
      `/v1/lending/pools/${POOL}/config-history`,
      { limit: 25, cursor: 'cur1' },
    );
    expect(screen.queryByRole('button', { name: 'Load more' })).toBeNull();
  });

  it('shows an empty state', async () => {
    vi.mocked(apiGet).mockResolvedValue({
      data: { pool: POOL, config_decimals: 7, changes: [] },
    });
    wrap(<ConfigHistory pool={POOL} />);
    expect(
      await screen.findByText(/No configuration changes recorded/),
    ).toBeInTheDocument();
  });
});
