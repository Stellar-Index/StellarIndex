import { describe, it, expect, beforeEach, vi } from 'vitest';
import { render, screen } from '@testing-library/react';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';

import { AccountRelationCohort } from './AccountRelationCohort';

const apiGet = vi.hoisted(() => vi.fn());
vi.mock('@/api/client', async (importOriginal) => {
  const mod = await importOriginal<typeof import('@/api/client')>();
  return { ...mod, apiGet };
});
vi.mock('@/components/charts/LineChart', () => ({
  LineChart: ({ ariaLabel }: { ariaLabel?: string }) => <div data-testid="line-chart">{ariaLabel}</div>,
}));

const ACCOUNT = 'GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN';
const USDC = 'USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN';

function cohort(overrides: Record<string, unknown> = {}) {
  return {
    account: ACCOUNT,
    relation: 'created',
    covered: true,
    cohort: { accounts: 1200, live_accounts: 900, active_30d: 40, active_90d: 90, active_365d: 300 },
    holdings: [
      { asset: 'native', kind: 'native', holders: 900, balance: '1250', price_usd: '0.10', value_usd: '125.00' },
      { asset: USDC, kind: 'classic', holders: 300, balance: '400', price_usd: '1', value_usd: '400.00' },
      { asset: 'pool:0a1b2c3d', kind: 'pool_share', holders: 5, balance: '7' },
    ],
    holdings_truncated: false,
    valuation: { total_usd: '525.00', priced_holdings: 2, unpriced_holdings: 1, basis: 'live_vwap_current' },
    flows: {
      granularity: '1M',
      assets: [USDC, 'native'],
      points: [
        {
          period: '2026-07',
          period_start: '2026-07-01T00:00:00Z',
          movements: 50,
          active_accounts: 21,
          by_asset: [
            { asset: USDC, inflow: '100', outflow: '25', scaled: true, inflow_usd: '100.00', outflow_usd: '25.00' },
            { asset: 'native', inflow: '3', outflow: '0', scaled: true, inflow_usd: '0.30', outflow_usd: '0.00' },
          ],
        },
      ],
    },
    contracts: [
      { contract_id: 'CBLENDPOOLXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXX', protocol: 'blend', movements: 40, active_accounts: 9, first_at: '2026-06-17T06:00:00Z', last_at: '2026-09-17T06:00:00Z' },
      { contract_id: 'CUNKNOWNXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXX', movements: 1, active_accounts: 1, first_at: '2026-09-17T06:00:00Z', last_at: '2026-09-17T06:00:00Z' },
    ],
    positions: [
      { protocol: 'blend', position_kind: 'lending_supply', venue: 'CBLENDPOOLXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXX', asset: 'CUSDC', asset_label: 'USDC', holders: 4, amount: '1234.5' },
    ],
    cycle: { computed_at: '2026-09-17T06:00:00Z', tip_ledger: 64400000 },
    note: 'n',
    ...overrides,
  };
}

function renderCohort(relation: 'created' | 'sponsored' = 'created') {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(
    <QueryClientProvider client={qc}>
      <AccountRelationCohort account={ACCOUNT} relation={relation} />
    </QueryClientProvider>,
  );
}

describe('AccountRelationCohort', () => {
  beforeEach(() => {
    apiGet.mockReset();
  });

  it('asks for the relation it was given and renders holdings, value, protocols and positions', async () => {
    apiGet.mockResolvedValueOnce({ data: cohort() });
    renderCohort('sponsored');
    expect(await screen.findByText('$525')).toBeInTheDocument();
    expect(apiGet.mock.calls[0]?.[0]).toBe(`/v1/accounts/${ACCOUNT}/graph/cohort?relation=sponsored`);
    expect(screen.getByText('1,200')).toBeInTheDocument();
    expect(screen.getByText('XLM')).toBeInTheDocument();
    expect(screen.getByText('USDC', { selector: 'span' })).toBeInTheDocument();
    expect(screen.getByText('liquidity-pool share')).toBeInTheDocument();
    expect(screen.getByText('unpriced')).toBeInTheDocument();
    // once as the labelled contract, once as the position's protocol
    expect(screen.getAllByText('blend', { selector: 'td' })).toHaveLength(2);
    expect(screen.getByText('unlabelled')).toBeInTheDocument();
    expect(screen.getByText('lending supply')).toBeInTheDocument();
    expect(screen.getAllByTestId('line-chart')).toHaveLength(2);
  });

  it('says a root the rollup does not carry is uncovered, never an empty cohort', async () => {
    apiGet.mockResolvedValueOnce({
      data: cohort({ covered: false, cohort: undefined, holdings: [], contracts: [], positions: [], flows: { granularity: '1M', assets: [], points: [] } }),
    });
    renderCohort();
    expect(await screen.findByText(/does not carry this creator/)).toBeInTheDocument();
    expect(screen.queryByText('Value moved by month')).not.toBeInTheDocument();
    expect(screen.getByText(/64,400,000/)).toBeInTheDocument();
  });

  it('reads a 503 as the rollup still warming', async () => {
    apiGet.mockRejectedValueOnce(new Error('503 Service Unavailable'));
    renderCohort();
    expect(await screen.findByText(/has not completed its first cycle/)).toBeInTheDocument();
  });

  it('draws no USD line when nothing moved is priced', async () => {
    const c = cohort();
    (c.flows.points[0] as { by_asset: unknown }).by_asset = [
      { asset: 'CTOKEN', inflow: '5000', outflow: '0', scaled: false },
    ];
    apiGet.mockResolvedValueOnce({ data: c });
    renderCohort();
    expect(await screen.findByText('No priced asset moved')).toBeInTheDocument();
    expect(screen.getAllByTestId('line-chart')).toHaveLength(1);
  });
});
