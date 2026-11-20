import { describe, it, expect, vi, beforeEach } from 'vitest';
import { render, screen, waitFor } from '@testing-library/react';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';

import { InlineMeter } from '@/components/charts/Bars';
import { metricValue, type HistoryPoint } from '../lendingInsights';
import { curveRateAt } from './ReserveDetail';
import {
  EmissionsPanel,
  emissionBars,
  emissionRows,
  perDayBase,
  type Emissions,
} from './EmissionsPanel';

vi.mock('@/api/client', async () => {
  const actual =
    await vi.importActual<typeof import('@/api/client')>('@/api/client');
  return { ...actual, apiGet: vi.fn() };
});
import { apiGet } from '@/api/client';

beforeEach(() => {
  vi.mocked(apiGet).mockReset();
});

const hp = (over: Partial<HistoryPoint>): HistoryPoint => ({
  t: '2026-11-01T00:00:00Z',
  removed: false,
  supplied: null,
  borrowed: null,
  utilization_pct: null,
  borrow_apr: null,
  supply_apr: null,
  collateral_factor: '9500000',
  liability_factor: '10000000',
  supply_cap: '100000000000',
  ...over,
});

describe('reserve history metrics', () => {
  it('scales factors by config decimals and the cap by token decimals', () => {
    expect(metricValue(hp({}), 'collateral_factor', 7, 7)).toBeCloseTo(95);
    expect(metricValue(hp({}), 'liability_factor', 7, 7)).toBeCloseTo(100);
    expect(metricValue(hp({}), 'supply_cap', 7, 7)).toBe(10000);
  });
  it('leaves a gap for an unlimited cap or unknown scale, never a spike or zero', () => {
    expect(
      metricValue(
        hp({ supply_cap: '170141183460469231731687303715884105727' }),
        'supply_cap',
        7,
        7,
      ),
    ).toBeNull();
    expect(metricValue(hp({}), 'collateral_factor', 7, null)).toBeNull();
  });
});

describe('rate curve', () => {
  it('interpolates the rate at current utilisation', () => {
    const curve = [
      { util: 0n, rate: 0n },
      { util: 5_000_000n, rate: 1_000_000n },
    ];
    expect(curveRateAt(curve, 2_500_000)).toBe(500_000);
    expect(curveRateAt(curve, 6_000_000)).toBeNull();
  });
});

describe('InlineMeter', () => {
  it('clamps and exposes the share', () => {
    render(<InlineMeter frac={1.4} label="USDC utilisation" />);
    expect(
      screen.getByRole('meter', { name: 'USDC utilisation' }),
    ).toHaveAttribute('aria-valuenow', '100');
  });
});

const emissions = (over: Partial<Emissions> = {}): Emissions =>
  ({
    pool: 'P',
    protocol: 'blnt',
    emission_token: 'native',
    emission_token_decimals: 7,
    eps_decimals: 7,
    storage_available: true,
    reserves: [
      {
        token_id: 1,
        asset: 'native',
        side: 'supply',
        eps: '1157407407407',
        expiration: '',
        last_update: '',
        live: true,
        archived: false,
      },
      {
        token_id: 0,
        asset: 'native',
        side: 'borrow',
        eps: '5',
        expiration: '',
        last_update: '',
        live: false,
        archived: false,
      },
    ],
    backstop: [
      {
        tier: 'FirstLoss',
        eps: '578703703703',
        expiration: '',
        last_update: '',
        live: true,
        archived: false,
      },
    ],
    claims: {
      available: false,
      pool: null,
      backstop: null,
      lower_bound: false,
    },
    ...over,
  }) as Emissions;

describe('emissions', () => {
  it('converts eps to tokens per day exactly', () => {
    // 1157407407407 / 1e7 base units/s * 86400 = 9999999999.996, floored
    expect(perDayBase('1157407407407', 7)).toBe('9999999999');
  });
  it('charts live streams only and labels sides and tiers', () => {
    const rows = emissionRows(emissions());
    expect(rows.map((r) => r.label)).toEqual([
      'XLM suppliers',
      'XLM borrowers',
      'Backstop, first loss',
    ]);
    const bars = emissionBars(rows, 7);
    expect(bars.map((b) => b.id)).toEqual(['r1', 'bFirstLoss']);
    expect(bars[0].title).toBe('999.9999999 per day');
    expect(emissionBars(rows, null)).toEqual([]);
  });
  it('renders the panel, the unavailable state and the error state', async () => {
    const client = () =>
      new QueryClient({ defaultOptions: { queries: { retry: false } } });
    vi.mocked(apiGet).mockResolvedValueOnce({ data: emissions() });
    const { unmount } = render(
      <QueryClientProvider client={client()}>
        <EmissionsPanel pool="P" />
      </QueryClientProvider>,
    );
    await waitFor(() =>
      expect(screen.getByLabelText(/per day by stream/)).toBeInTheDocument(),
    );
    unmount();
    vi.mocked(apiGet).mockResolvedValueOnce({
      data: emissions({
        storage_available: false,
        storage_unavailable_reason: 'blend_v1_emission_storage_unverified',
        reserves: [],
        backstop: [],
      }),
    });
    const r2 = render(
      <QueryClientProvider client={client()}>
        <EmissionsPanel pool="P" />
      </QueryClientProvider>,
    );
    await waitFor(() =>
      expect(screen.getByText(/Blend v1 emission storage/)).toBeInTheDocument(),
    );
    r2.unmount();
    vi.mocked(apiGet).mockRejectedValueOnce(new Error('boom'));
    render(
      <QueryClientProvider client={client()}>
        <EmissionsPanel pool="P" />
      </QueryClientProvider>,
    );
    await waitFor(() =>
      expect(screen.getByRole('alert')).toHaveTextContent(/unavailable/),
    );
  });
});
