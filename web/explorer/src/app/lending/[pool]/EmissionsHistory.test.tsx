import { describe, it, expect, vi, beforeEach } from 'vitest';
import { fireEvent, render, screen, waitFor } from '@testing-library/react';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';

import {
  EmissionsHistory,
  emissionStreams,
  insightText,
  type EmissionHistory,
} from './EmissionsHistory';

vi.mock('@/api/client', async () => {
  const actual =
    await vi.importActual<typeof import('@/api/client')>('@/api/client');
  return { ...actual, apiGet: vi.fn() };
});

import { apiGet } from '@/api/client';

const POOL = 'CAJJZSGMMM3PD7N33TAPHGBUGTB43OC73HVIK2L2G6BNGGGYOSSYBXBD';
const BLND_SAC = 'CD25MNVTZDL4Y3XBCPCJXGXATV5WUHHOWMYFF4YBEGU5FCPGMYTVG5JY';
const ISSUER = 'GDJEHTBE6ZHUXSWFI642DCGLUOECLHPF3KSXHPXTSTJ7E3JF6MQ5EZYY';
const USDC_SAC = 'CCW67TSZV3SSS2HXMBQ5JFGCKJNXKZM7UQUWUZPUTHXSTZLEO7SJMI75';
const D1 = '2026-11-01T00:00:00Z';
const D2 = '2026-11-02T00:00:00Z';
const D3 = '2026-11-03T00:00:00Z';
const END = '2026-11-04T00:00:00Z';

type O = Record<string, unknown>;

const usd = (amount: string | null, o: O = {}) => ({
  amount_usd: amount,
  priced_hours: 24,
  unpriced_hours: 0,
  price_carried_hours: 0,
  lower_bound: false,
  usd_reason: null,
  ...o,
});

const pt = (t: string, tokens: string, u: ReturnType<typeof usd>) => ({
  t,
  partial: false,
  tokens,
  usd: u,
});

function history(o: O = {}): EmissionHistory {
  return {
    pool: POOL,
    protocol: 'blnt',
    interval: '1d',
    from: D1,
    to: END,
    served_through: END,
    emission_token: BLND_SAC,
    emission_token_decimals: 7,
    eps_decimals: 7,
    price_max_age_hours: 6,
    lower_bound: false,
    excluded: [],
    reserves: {
      available: true,
      unavailable_reason: null,
      lower_bound: false,
      served_through: END,
      approximate: false,
      approximate_reason: null,
      streams: [
        {
          token_id: 1,
          asset: USDC_SAC,
          side: 'supply',
          tokens: '20000000',
          approximate: false,
          approximate_reason: null,
          usd: usd('2.00'),
          points: [
            pt(D1, '10000000', usd('1.00')),
            pt(D2, '10000000', usd('1.00')),
          ],
        },
        {
          token_id: 0,
          asset: USDC_SAC,
          side: 'borrow',
          tokens: '5000000',
          approximate: false,
          approximate_reason: null,
          usd: usd('0.50'),
          points: [pt(D1, '5000000', usd('0.50'))],
        },
      ],
    },
    pool_claims: {
      available: true,
      unavailable_reason: null,
      served_through: END,
      lower_bound: false,
      totals: { claims: 3, amount: '20000000' },
      usd: usd('2.00'),
      points: [
        {
          t: D2,
          partial: false,
          claimers: 2,
          claims: 3,
          amount: '20000000',
          usd: usd('2.00'),
        },
      ],
      by_reserve: { lower_bound: false, reserves: [] },
    },
    backstop_alloc: {
      available: true,
      unavailable_reason: null,
      served_through: END,
      lower_bound: false,
      tier_split_available: false,
      tier_split_reason: 'tier_split_not_on_chain',
      totals: { events: 0, backstop_emissions: '0', pool_emissions: '0' },
      usd: null,
      points: [],
    },
    backstop_claims: {
      available: true,
      unavailable_reason: null,
      served_through: END,
      lower_bound: false,
      units: { blnt_amount: 'blnt', lp_amount: 'lp', shares: 'shares' },
      tiers: [
        {
          tier: 'FirstLoss',
          totals: {
            claims: 1,
            blnt_amount: '30000000',
            lp_amount: '0',
            shares: '0',
          },
          usd: usd('3.00'),
          points: [
            {
              t: D3,
              partial: false,
              claimers: 1,
              claims: 1,
              blnt_amount: '30000000',
              lp_amount: '0',
              shares: '0',
              usd: usd('3.00'),
            },
          ],
        },
      ],
    },
    ...o,
  } as EmissionHistory;
}

/** History with a partly-valued schedule day and an unvalued claim day. */
function partlyPriced(): EmissionHistory {
  const d = history();
  d.reserves.streams[0].points[1].usd = usd('1.00', {
    lower_bound: true,
    unpriced_hours: 3,
  });
  d.reserves.streams[0].usd = usd('2.00', { lower_bound: true });
  d.pool_claims.points[0].usd = usd(null, {
    priced_hours: 0,
    usd_reason: 'withheld',
  });
  d.pool_claims.usd = usd(null, { priced_hours: 0, usd_reason: 'withheld' });
  return d;
}

const tokenText = (raw: string) => `${raw} BLND`;

/** `data` as a string is the error message the history fetch fails with. */
function renderWith(data: EmissionHistory | string) {
  vi.mocked(apiGet).mockImplementation(async (path: string) => {
    if (path === '/v1/sac-wrappers')
      return { data: { [BLND_SAC]: `BLND-${ISSUER}` } } as never;
    if (typeof data === 'string') throw new Error(data);
    return { data } as never;
  });
  return render(
    <QueryClientProvider
      client={
        new QueryClient({ defaultOptions: { queries: { retry: false } } })
      }
    >
      <EmissionsHistory pool={POOL} />
    </QueryClientProvider>,
  );
}

const marks = (key: string, mark: string) =>
  [
    ...screen
      .getByTestId(`emissions-stream-${key}`)
      .querySelectorAll(`[data-mark="${mark}"]`),
  ].map((el) => el.querySelector('title')?.textContent);

beforeEach(() => {
  vi.mocked(apiGet).mockReset();
});

describe('emissionStreams', () => {
  it('maps each stream to one exact cell per bucket, an empty bucket as zero', () => {
    const [sched, pool, backstop] = emissionStreams(history());
    expect(sched.cells.map((c) => [c.tokens, c.usd])).toEqual([
      ['15000000', '1.50'],
      ['10000000', '1.00'],
      ['0', '0'],
    ]);
    expect(pool.cells.map((c) => c.tokens)).toEqual(['0', '20000000', '0']);
    expect(backstop.cells.map((c) => c.usd)).toEqual(['0', '0', '3.00']);
    expect(sched.totalTokens).toBe('25000000');
    expect(sched.totalUsd).toBe('2.50');
  });

  it('leaves a bucket past served_through unknown, never zero', () => {
    const d = history();
    d.pool_claims.served_through = D3;
    const pool = emissionStreams(d)[1];
    expect(pool.cells[2]).toMatchObject({ tokens: null, usd: null });
  });

  it('marks a partly valued bucket a lower bound and an unvalued one null with its reason', () => {
    const [sched, pool] = emissionStreams(partlyPriced());
    expect(sched.cells[1]).toMatchObject({ usd: '1.00', usdLowerBound: true });
    expect(pool.cells[1]).toMatchObject({
      tokens: '20000000',
      usd: null,
      reason: 'withheld',
    });
  });
});

describe('insightText', () => {
  it('states exact totals when every bucket is exact', () => {
    expect(
      insightText(emissionStreams(history()), 'usd', '90 days', tokenText),
    ).toBe(
      'Over 90 days: $2.50 scheduled to the reserves; $2.00 claimed from the pool; $3.00 claimed by backstop depositors.',
    );
  });

  it('says "at least" for a stream with any partial bucket', () => {
    const text = insightText(
      emissionStreams(partlyPriced()),
      'usd',
      '90 days',
      tokenText,
    );
    expect(text).toContain('at least $2.50 scheduled');
    // Nothing valued: the exact tokens, labelled as unpriced, never "$0".
    expect(text).toContain(
      '; 20000000 BLND (not priced) claimed from the pool',
    );
    expect(text).toContain('; $3.00 claimed by backstop');
  });

  it('says "at least" when part of the range is not served yet', () => {
    const d = history();
    d.backstop_claims.served_through = D2;
    expect(
      insightText(emissionStreams(d), 'tokens', '90 days', tokenText),
    ).toContain('at least 30000000 BLND claimed by backstop');
  });

  it('says "at least" when the stream itself is a lower bound', () => {
    const d = history({
      excluded: [
        {
          stream: 'reserves',
          reason: 'history_unverified',
          count: null,
          amount: null,
        },
      ],
    });
    d.reserves.lower_bound = true;
    const [sched] = emissionStreams(d);
    expect(sched.lowerBound).toEqual([
      'history not yet verified back to the first ledger',
    ]);
    expect(insightText([sched], 'tokens', '30 days', tokenText)).toBe(
      'Over 30 days: at least 25000000 BLND scheduled to the reserves.',
    );
  });
});

describe('EmissionsHistory', () => {
  it('draws partial buckets distinctly with the reason in the tooltip', async () => {
    renderWith(partlyPriced());
    await screen.findByTestId('emissions-insight');
    expect(marks('schedule', 'lower-bound')).toEqual([
      '2026-11-02 — Scheduled: at least $1 (some hours had no USD price)',
    ]);
    expect(marks('pool', 'missing')).toEqual([
      '2026-11-02: no value (no USD value: price withheld (flagged issuer or thin market))',
    ]);
    // Exact buckets carry neither mark.
    expect(marks('backstop', 'lower-bound')).toEqual([]);
    expect(marks('backstop', 'missing')).toEqual([]);
  });

  it('switches to token units named by the token', async () => {
    renderWith(partlyPriced());
    const btn = await screen.findByRole('button', { name: 'BLND' });
    fireEvent.click(btn);
    await waitFor(() =>
      expect(screen.getByTestId('emissions-insight')).toHaveTextContent(
        'Over 90 days: 2.5 BLND scheduled to the reserves; 2 BLND claimed from the pool; 3 BLND claimed by backstop depositors.',
      ),
    );
    // Tokens are exact in every bucket, so nothing is drawn as unknown.
    expect(marks('pool', 'missing')).toEqual([]);
    const titles = [
      ...screen
        .getByTestId('emissions-stream-pool')
        .querySelectorAll('rect > title'),
    ].map((t) => t.textContent);
    expect(titles).toContain('2026-11-02 — Claimed from the pool: 2 BLND');
  });

  it('shows an empty state for a pool with no emissions', async () => {
    const d = history();
    d.reserves.streams = [];
    d.pool_claims.points = [];
    d.backstop_claims.tiers = [];
    renderWith(d);
    expect(await screen.findByRole('status')).toHaveTextContent(
      'No emissions in this range.',
    );
    expect(screen.queryByTestId('emissions-insight')).toBeNull();
  });

  it('names why a stream is not shown', async () => {
    const d = history({ protocol: 'blend' });
    d.backstop_claims.available = false;
    d.backstop_claims.unavailable_reason = 'backstop_claim_names_no_pool';
    d.backstop_claims.served_through = null;
    renderWith(d);
    expect(
      await screen.findByText(
        /Claimed by backstop depositors: Blend backstop claims/,
      ),
    ).toBeInTheDocument();
    expect(screen.queryByTestId('emissions-stream-backstop')).toBeNull();
  });

  it('shows the error state with a retry', async () => {
    renderWith('503 unavailable');
    expect(await screen.findByRole('alert')).toHaveTextContent(
      'Emission history is unavailable right now.',
    );
    expect(screen.getByRole('button', { name: 'Retry' })).toBeInTheDocument();
  });

  it('renders nothing for a contract that is not a known pool', async () => {
    const { container } = renderWith('404 not found');
    await waitFor(() => expect(container).toBeEmptyDOMElement());
  });
});
