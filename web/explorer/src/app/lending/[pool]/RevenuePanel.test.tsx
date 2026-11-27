import { describe, it, expect, vi, beforeEach } from 'vitest';
import { fireEvent, render, screen, waitFor } from '@testing-library/react';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';

import { RevenuePanel, revenueBuckets, type Revenue } from './RevenuePanel';

vi.mock('@/api/client', async () => {
  const actual =
    await vi.importActual<typeof import('@/api/client')>('@/api/client');
  return { ...actual, apiGet: vi.fn() };
});

import { apiGet } from '@/api/client';

const POOL = 'CAJJZSGMMM3PD7N33TAPHGBUGTB43OC73HVIK2L2G6BNGGGYOSSYBXBD';
const USDC_SAC = 'CCW67TSZV3SSS2HXMBQ5JFGCKJNXKZM7UQUWUZPUTHXSTZLEO7SJMI75';
const EURC_SAC = 'CDTKPWPLOURQA2SGTKTUQOWRCBZEORB4BWBOMJ3D3ZTQQSGE5F6JBQLV';

type O = Record<string, unknown>;

const usd = (o: O = {}) => ({
  interest_accrued_usd: '125.00',
  supplier_share_usd: '100.00',
  supplier_credited_usd: '100.00',
  backstop_take_usd: '25.00',
  protocol_fee_usd: '0.00',
  backstop_drawn_usd: '5.00',
  default_loss_usd: '0.00',
  priced_hours: 48,
  unpriced_hours: 0,
  price_carried_hours: 0,
  lower_bound: false,
  usd_reason: null,
  ...o,
});

const amounts = (o: O = {}) => ({
  interest_accrued: '1250000000',
  supplier_share: '1000000000',
  supplier_credited: '1000000000',
  backstop_take: '250000000',
  protocol_fee: '0',
  realised: {
    backstop_drawn: '50000000',
    backstop_drawn_lower_bound: true,
    fee_withdrawn: null,
    fee_withdrawn_reason: 'not_applicable',
  },
  backstop_gulped: '0',
  reconcile_backstop_loss: '0',
  default_loss: '0',
  default_versions: 0,
  unexplained_supply_loss: '0',
  unexplained_supply_loss_versions: 0,
  unexplained_supply_gain: '0',
  unexplained_supply_gain_versions: 0,
  unexplained_backstop_residual: '0',
  unexplained_versions: 0,
  unordered_versions: 0,
  accrual_errors: 0,
  ...o,
});

const reserve = (o: O = {}) => ({
  asset: USDC_SAC,
  decimals: 7,
  fee_rate_inferred: false,
  lower_bound: false,
  totals: amounts(),
  usd: usd(),
  points: [
    {
      t: '2026-11-14T00:00:00Z',
      partial: false,
      usd: usd({
        interest_accrued_usd: '100.00',
        supplier_share_usd: '80.00',
        backstop_take_usd: '20.00',
      }),
      ...amounts(),
    },
    {
      t: '2026-11-15T00:00:00Z',
      partial: true,
      usd: usd({
        interest_accrued_usd: '25.00',
        supplier_share_usd: '20.00',
        backstop_take_usd: '5.00',
      }),
      ...amounts(),
    },
  ],
  ...o,
});

function payload(o: O = {}, totals: O = {}) {
  return {
    data: {
      pool: POOL,
      protocol: 'blend',
      interval: '1d',
      from: '2026-08-18T00:00:00Z',
      to: '2026-11-16T00:00:00Z',
      coverage: {
        derive_rev: 4,
        from_ledger: 51499541,
        through_ledger: 63339779,
        through_hour: '2026-11-15T22:00:00Z',
      },
      lower_bound: false,
      excluded: [],
      price_max_age_hours: 6,
      totals_usd: { ...usd(), excluded: [], ...totals },
      reserves: [reserve()],
      ...o,
    },
  };
}

// The pool's derive revision moved on: every reserve null, named derive_stale.
const STALE = payload(
  {
    lower_bound: true,
    excluded: [
      {
        kind: 'reserve',
        asset: USDC_SAC,
        reason: 'derive_stale',
        amount: null,
        count: 2160,
      },
    ],
    reserves: [
      reserve({
        lower_bound: true,
        totals: null,
        points: [],
        usd: usd({
          interest_accrued_usd: null,
          supplier_share_usd: null,
          supplier_credited_usd: null,
          backstop_take_usd: null,
          protocol_fee_usd: null,
          backstop_drawn_usd: null,
          default_loss_usd: null,
          priced_hours: 0,
          lower_bound: true,
          usd_reason: 'amounts_excluded',
        }),
      }),
    ],
  },
  {
    interest_accrued_usd: null,
    supplier_share_usd: null,
    backstop_take_usd: null,
    backstop_drawn_usd: null,
    default_loss_usd: null,
    lower_bound: true,
    usd_reason: 'price_missing',
  },
);

function mockApi(revenue: (q: O) => unknown) {
  vi.mocked(apiGet).mockImplementation(async (path: string, q?: unknown) => {
    if (path === '/v1/sac-wrappers')
      return {
        data: {
          [USDC_SAC]:
            'USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN',
          [EURC_SAC]:
            'EURC-GDHU6WRG4IEQXM5NZ4BMPKOXHW76MZM4Y2IEMFDVXBSDP6SJY4ITNPP2',
        },
      };
    return revenue((q ?? {}) as O);
  });
}

function renderIt() {
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  });
  return render(
    <QueryClientProvider client={client}>
      <RevenuePanel pool={POOL} />
    </QueryClientProvider>,
  );
}

/** The value element of the Stat labelled `label`. */
const stat = (label: string) =>
  screen.getByText(label, { selector: 'span' }).parentElement
    ?.nextElementSibling as HTMLElement;

beforeEach(() => {
  vi.mocked(apiGet).mockReset();
});

describe('RevenuePanel', () => {
  it('a clean pool: exact headline, split, no "at least" except the paid-out floor', async () => {
    mockApi(() => payload());
    renderIt();
    await screen.findByText('Interest accrued');
    expect(stat('Interest accrued').textContent).toBe('$125.00');
    expect(stat('Supplier share').textContent).toBe('$100.00');
    expect(stat('Backstop take').textContent).toBe('$25.00');
    // Blend has no protocol fee leg.
    expect(screen.queryByText('Protocol fee', { selector: 'span' })).toBeNull();
    // backstop_drawn is a lower bound by definition, so it alone says so.
    expect(stat('Paid out to backstop').textContent).toBe('≥at least $5.00');
    expect(screen.getAllByText('at least')).toHaveLength(1);
    expect(screen.queryByTestId('revenue-excluded')).toBeNull();
    expect(screen.getByTestId(`revenue-status-${USDC_SAC}`).textContent).toBe(
      'complete',
    );
  });

  it('a lower-bound pool says "at least" and lists what it leaves out', async () => {
    mockApi(() =>
      payload(
        {
          lower_bound: true,
          excluded: [
            {
              kind: 'window',
              asset: null,
              reason: 'interest_not_derived',
              amount: null,
              count: null,
            },
          ],
          reserves: [reserve({ lower_bound: true })],
        },
        {
          lower_bound: true,
          excluded: [{ asset: EURC_SAC, reason: 'unpriced', count: 3 }],
        },
      ),
    );
    renderIt();
    await screen.findByText(/EURC: no USD market/);
    expect(stat('Interest accrued').textContent).toBe('≥at least $125.00');
    const left = screen.getByTestId('revenue-excluded').textContent;
    expect(left).toContain(
      'Part of this range: interest is not derived yet for part of this range',
    );
    expect(left).toContain('EURC: no USD market (3 hours not valued)');
    expect(
      stat('Interest accrued').querySelector('[title]')?.getAttribute('title'),
    ).toMatch(/^At least \$125\.00; leaves out .*EURC/);
  });

  it('an all-null derive_stale pool says it is being recomputed, never zeros or an empty chart', async () => {
    mockApi(() => STALE);
    const { container } = renderIt();
    await screen.findByText(/Revenue history is being recomputed/);
    expect(container.textContent).not.toMatch(/\$0/);
    expect(screen.queryByRole('table')).toBeNull();
    expect(screen.queryByRole('img')).toBeNull();
    expect(screen.queryByText('Interest accrued')).toBeNull();
  });

  it('a BLNT pool shows the protocol fee, its inferred rate and fee withdrawn as not derivable', async () => {
    const blnt = reserve({
      fee_rate_inferred: true,
      totals: amounts({
        protocol_fee: '125000000',
        realised: {
          backstop_drawn: '0',
          backstop_drawn_lower_bound: true,
          fee_withdrawn: null,
          fee_withdrawn_reason: 'not_derivable',
        },
      }),
      usd: usd({ protocol_fee_usd: '12.50' }),
    });
    mockApi(() =>
      payload(
        { protocol: 'blnt', reserves: [blnt] },
        { protocol_fee_usd: '12.50' },
      ),
    );
    renderIt();
    await screen.findByText('Interest accrued');
    expect(stat('Protocol fee').textContent).toBe('$12.50');
    expect(screen.getByText('Fee rate inferred')).toBeInTheDocument();
    expect(
      screen.getByText('Protocol fee withdrawn: not yet derivable.'),
    ).toBeInTheDocument();
  });

  it('a null USD value renders a dash with its reason, never $0', async () => {
    mockApi(() =>
      payload(
        {
          reserves: [
            reserve({
              usd: usd({
                interest_accrued_usd: null,
                supplier_share_usd: null,
                backstop_take_usd: null,
                usd_reason: 'withheld',
              }),
            }),
          ],
        },
        {
          interest_accrued_usd: null,
          supplier_share_usd: null,
          backstop_take_usd: null,
          usd_reason: 'price_missing',
        },
      ),
    );
    const { container } = renderIt();
    await screen.findByText('Interest accrued');
    expect(stat('Interest accrued').textContent).toBe('—no price');
    const cell = screen.getAllByTitle(
      'price withheld (flagged issuer or thin market)',
    );
    expect(cell.length).toBe(3);
    cell.forEach((c) => expect(c.textContent).toBe('—'));
    expect(container.textContent).not.toMatch(/\$0\.00(?!\d)/);
  });

  it('shows losses apart from interest, never netted', async () => {
    mockApi(() =>
      payload({
        reserves: [
          reserve({
            totals: amounts({
              unexplained_supply_loss: '5000000',
              unexplained_supply_loss_versions: 2,
            }),
          }),
        ],
      }),
    );
    renderIt();
    await screen.findByText('Interest accrued');
    expect(stat('Interest accrued').textContent).toBe('$125.00');
    await screen.findByText(/USDC over 2 rate changes/);
    expect(screen.getByTestId('revenue-losses').textContent).toContain(
      'supplier loss no event explains: 0.5 USDC over 2 rate changes',
    );
    expect(screen.getByTestId(`revenue-status-${USDC_SAC}`).textContent).toBe(
      'unexplained supplier loss',
    );
  });

  it('charts each day by leg and marks a partial day', async () => {
    mockApi(() => payload());
    const { container } = renderIt();
    await screen.findByText('Interest accrued');
    const titles = [...container.querySelectorAll('svg title')].map(
      (t) => t.textContent,
    );
    expect(titles).toContain('2026-11-14 — Supplier share: $80');
    expect(titles).toContain('2026-11-15 (partial) — Backstop take: $5');
    expect(screen.getByText(/1 bucket is partial/)).toBeInTheDocument();
  });

  it('refetches with the chosen window', async () => {
    const seen: O[] = [];
    mockApi((q) => {
      seen.push(q);
      return payload();
    });
    renderIt();
    await screen.findByText('Interest accrued');
    fireEvent.click(screen.getByText('30 days'));
    await waitFor(() =>
      expect(seen.some((q) => q.window === '30d')).toBe(true),
    );
    expect(seen[0]).toMatchObject({ window: '90d', interval: '1d' });
  });

  it('a failed fetch says so with a retry', async () => {
    mockApi(() => {
      throw new Error(
        '500 Internal Server Error on /v1/lending/pools/x/revenue',
      );
    });
    renderIt();
    await screen.findByText(/Revenue is unavailable right now/);
    expect(screen.getByRole('button', { name: 'Retry' })).toBeInTheDocument();
  });

  it('a 404 (not a pool the API derives) renders no panel', async () => {
    mockApi(() => {
      throw new Error('404 Not Found on /v1/lending/pools/x/revenue');
    });
    const { container } = renderIt();
    await waitFor(() => expect(container.textContent).toBe(''));
  });
});

describe('revenueBuckets', () => {
  it('sums legs exactly across reserves and marks unvalued interest as a lower bound', () => {
    const d = payload().data as unknown as Revenue;
    const a = d.reserves[0];
    const b = {
      ...a,
      asset: EURC_SAC,
      points: [
        {
          ...a.points[0],
          usd: { ...a.points[0].usd, supplier_share_usd: '0.10' },
        },
        {
          ...a.points[1],
          usd: { ...a.points[1].usd, interest_accrued_usd: null },
        },
      ],
    };
    const out = revenueBuckets([a, b]);
    expect(
      out.map((x) => [x.t.slice(0, 10), x.supplier, x.lowerBound]),
    ).toEqual([
      ['2026-11-14', '80.10', false],
      ['2026-11-15', '20.00', true],
    ]);
  });
});
