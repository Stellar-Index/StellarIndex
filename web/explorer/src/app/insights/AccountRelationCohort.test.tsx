import { describe, it, expect, beforeEach, vi } from 'vitest';
import { fireEvent, render, screen } from '@testing-library/react';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';

import { AccountRelationCohort } from './AccountRelationCohort';

const apiGet = vi.hoisted(() => vi.fn());
vi.mock('@/api/client', async (importOriginal) => {
  const mod = await importOriginal<typeof import('@/api/client')>();
  return { ...mod, apiGet };
});
type MockSeries = {
  label: string;
  data: { time: number; value: number | null }[];
};
vi.mock('@/components/charts/LineChart', () => ({
  LineChart: ({
    ariaLabel,
    series,
  }: {
    ariaLabel?: string;
    series?: MockSeries[];
  }) => (
    <div
      data-testid="line-chart"
      data-series={
        series
          ? JSON.stringify(
              series.map((s) => [s.label, s.data.map((p) => p.value)]),
            )
          : undefined
      }
    >
      {ariaLabel}
    </div>
  ),
}));

const ACCOUNT = 'GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN';
const USDC = 'USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN';

const valuation = (over: Record<string, unknown> = {}) => ({
  total_usd: '525.00',
  priced_holdings: 2,
  unpriced_holdings: 1,
  price_cap: 100,
  unpriced_over_cap: 0,
  basis: 'live_vwap_current',
  ...over,
});

const CBLEND = 'CBLENDPOOLXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXX';
const CUNKNOWN = 'CUNKNOWNXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXX';

type Pair = [string, string];
// USD figures on the today basis, the then basis, and the then price.
const usd = (now?: Pair, then?: Pair, price?: string) => ({
  ...(now && { inflow_usd: now[0], outflow_usd: now[1] }),
  ...(then && { inflow_usd_then: then[0], outflow_usd_then: then[1] }),
  ...(price && { price_usd_then: price }),
});
const flowRow = (
  asset: string,
  inflow: string,
  outflow: string,
  figures: object = {},
) => ({ asset, inflow, outflow, scaled: true, ...figures });
const contract = (contract_id: string, extra: object, first_at: string) => ({
  contract_id,
  active_accounts: 1,
  first_at,
  last_at: '2026-09-17T06:00:00Z',
  ...extra,
});

function cohort(overrides: Record<string, unknown> = {}) {
  return {
    account: ACCOUNT,
    relation: 'created',
    covered: true,
    cohort: {
      accounts: 1200,
      live_accounts: 900,
      active_30d: 40,
      active_90d: 90,
      active_365d: 300,
    },
    holdings: [
      {
        asset: 'native',
        kind: 'native',
        holders: 900,
        balance: '1250',
        price_usd: '0.10',
        value_usd: '125.00',
      },
      {
        asset: USDC,
        kind: 'classic',
        holders: 300,
        balance: '400',
        price_usd: '1',
        value_usd: '400.00',
      },
      { asset: 'pool:0a1b2c3d', kind: 'pool_share', holders: 5, balance: '7' },
    ],
    holdings_truncated: false,
    valuation: valuation(),
    flows: {
      granularity: '1M',
      assets: [USDC, 'native'],
      points: [
        {
          period: '2026-07',
          period_start: '2026-07-01T00:00:00Z',
          movements: 50,
          active_accounts: 21,
          priced_assets: 2,
          ...usd(['100.30', '25.00'], ['100.80', '24.95']),
          by_asset: [
            flowRow(
              USDC,
              '100',
              '25',
              usd(['100.00', '25.00'], ['99.80', '24.95'], '0.998'),
            ),
            flowRow(
              'native',
              '3',
              '0',
              usd(['0.30', '0.00'], ['1.00', '0.00'], '0.3333333333'),
            ),
          ],
        },
      ],
    },
    contracts: [
      contract(
        CBLEND,
        { protocol: 'blend', movements: 40, active_accounts: 9 },
        '2026-06-17T06:00:00Z',
      ),
      contract(CUNKNOWN, { movements: 1 }, '2026-09-17T06:00:00Z'),
    ],
    positions: [
      {
        protocol: 'blend',
        position_kind: 'lending_supply',
        venue: CBLEND,
        asset: 'CUSDC',
        asset_label: 'USDC',
        holders: 4,
        amount: '1234.5',
      },
    ],
    cycle: { computed_at: '2026-09-17T06:00:00Z', tip_ledger: 64400000 },
    note: 'n',
    ...overrides,
  };
}

const serve = (data: unknown) => apiGet.mockResolvedValueOnce({ data });

/** Empties a month point's basket: no point USD figure on either basis. */
function emptyBasket(p: Record<string, unknown>, byAsset: unknown[]) {
  for (const k of [
    'inflow_usd',
    'outflow_usd',
    'inflow_usd_then',
    'outflow_usd_then',
  ])
    delete p[k];
  p.priced_assets = 0;
  p.by_asset = byAsset;
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
    serve(cohort());
    renderCohort('sponsored');
    expect(await screen.findByText('$525.00')).toBeInTheDocument();
    expect(apiGet.mock.calls[0]?.[0]).toBe(
      `/v1/accounts/${ACCOUNT}/graph/cohort?relation=sponsored`,
    );
    // A complete priced total is served as-is, with no lower-bound caveat.
    expect(screen.getByText('2 priced · 1 no live price')).toBeInTheDocument();
    expect(
      screen.queryByText('The priced total is a lower bound'),
    ).not.toBeInTheDocument();
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
    serve(
      cohort({
        covered: false,
        cohort: undefined,
        holdings: [],
        contracts: [],
        positions: [],
        flows: { granularity: '1M', assets: [], points: [] },
      }),
    );
    renderCohort();
    expect(
      await screen.findByText(/does not carry this creator/),
    ).toBeInTheDocument();
    expect(screen.queryByText('Value moved by month')).not.toBeInTheDocument();
    expect(screen.getByText(/64,400,000/)).toBeInTheDocument();
  });

  it.each([
    [
      'a total cut short by the request deadline as a lower bound and says why',
      { priced_holdings: 3, unpriced_holdings: 40, degraded: true },
      '3 priced · 40 unpriced or not reached',
      /cut short by the request deadline/,
      /Only the 100 largest/,
    ],
    [
      'holdings outside the price cap as not looked up, not as having no live price',
      { unpriced_holdings: 6, unpriced_over_cap: 5 },
      '2 priced · 1 no live price · 5 not looked up',
      /Only the 100 largest holdings are priced per request; 5 smaller holdings were not looked up/,
      /cut short/,
    ],
  ])('renders %s', async (_name, over, counts, shown, hidden) => {
    serve(cohort({ valuation: valuation(over) }));
    renderCohort();
    expect(await screen.findByText('≥ $525.00')).toBeInTheDocument();
    expect(screen.queryByText('$525.00')).not.toBeInTheDocument();
    expect(screen.getByText(counts)).toBeInTheDocument();
    expect(
      screen.getByText('The priced total is a lower bound'),
    ).toBeInTheDocument();
    expect(screen.getByText(shown)).toBeInTheDocument();
    expect(screen.queryByText(hidden)).not.toBeInTheDocument();
  });

  it('reads a 503 as the rollup still warming', async () => {
    apiGet.mockRejectedValueOnce(new Error('503 Service Unavailable'));
    renderCohort();
    expect(
      await screen.findByText(/has not completed its first cycle/),
    ).toBeInTheDocument();
  });

  it.each([
    [
      'nothing moved is priced',
      [{ ...flowRow('CTOKEN', '5000', '0'), scaled: false }],
    ],
    [
      // Today only, then only: each row keeps its figure, the month has no basket.
      'no asset is priced on both bases, however each is priced alone',
      [
        flowRow(USDC, '100', '25', usd(['100.00', '25.00'])),
        flowRow(
          'native',
          '3',
          '0',
          usd(undefined, ['1.00', '0.00'], '0.3333333333'),
        ),
      ],
    ],
  ])('draws no USD line when %s', async (_name, byAsset) => {
    const c = cohort();
    emptyBasket(c.flows.points[0]!, byAsset);
    serve(c);
    renderCohort();
    expect(
      await screen.findByText('No priced asset moved'),
    ).toBeInTheDocument();
    expect(screen.getAllByTestId('line-chart')).toHaveLength(1);
    expect(screen.queryByText('USD then')).not.toBeInTheDocument();
  });

  it('draws both bases from the served month sums, one basket, never a re-sum of the rows', async () => {
    const c = cohort();
    // A row priced today only is outside the basket the point sums: were
    // the chart to re-sum by_asset it would read 150.30 today.
    (c.flows.points[0]!.by_asset as unknown[]).push(
      flowRow('AQUA-GISSUER', '50', '0', usd(['50.00', '0.00'])),
    );
    serve(c);
    renderCohort();
    await screen.findByText('$525.00');

    const chart = () => screen.getAllByTestId('line-chart')[1]!;
    const series = () =>
      JSON.parse(chart().getAttribute('data-series') ?? '[]') as [
        string,
        number[],
      ][];

    const todayPill = screen.getByText('USD today');
    const thenPill = screen.getByText('USD then');
    expect(todayPill).toHaveAttribute('aria-pressed', 'true');
    expect(thenPill).toHaveAttribute('aria-pressed', 'false');
    expect(chart()).toHaveTextContent("at today's prices");
    expect(series()).toEqual([
      ['Moved in', [100.3]],
      ['Moved out', [25]],
    ]);

    fireEvent.click(thenPill);
    expect(thenPill).toHaveAttribute('aria-pressed', 'true');
    expect(todayPill).toHaveAttribute('aria-pressed', 'false');
    expect(chart()).toHaveTextContent("at each month's own prices");
    expect(series()).toEqual([
      ['Moved in', [100.8]],
      ['Moved out', [24.95]],
    ]);

    fireEvent.click(todayPill);
    expect(chart()).toHaveTextContent("at today's prices");
  });
});
