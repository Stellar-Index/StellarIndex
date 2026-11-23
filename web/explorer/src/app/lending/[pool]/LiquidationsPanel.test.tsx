import { describe, it, expect, vi, beforeEach } from 'vitest';
import { fireEvent, render, screen, within } from '@testing-library/react';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';

import { LiquidationsPanel } from './LiquidationsPanel';
import type { Liquidation, LiquidationLeg } from '../lendingLiquidations';

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
const XLM = 'CAS3J7GYLGXMF6TDJBBYYSE3HQ6BBSMLNUQ34T6TZMYMW2EVH34XOWMA';
const USDC = 'CCW67TSZV3SSS2HXMBQ5JFGCKJNXKZM7UQUWUZPUTHXSTZLEO7SJMI75';
const USER = 'G'.padEnd(56, 'A');
const FILLER = 'G'.padEnd(56, 'B');

function leg(over: Partial<LiquidationLeg>): LiquidationLeg {
  return {
    asset: XLM,
    kind: 'b_token',
    amount: '1000000000',
    underlying: '1100000000',
    price_usd: '0.30',
    price_at: '2026-07-03T22:00:00Z',
    usd: '33.00',
    reason: null,
    ...over,
  };
}

function fill(over: Partial<Liquidation>): Liquidation {
  return {
    auction_type: 0,
    auction_type_label: 'user_liquidation',
    user: USER,
    filler: FILLER,
    fill_percent: '100',
    ledger: 100,
    time: '2026-07-03T22:57:00Z',
    tx_hash: 'a'.repeat(64),
    block: 90,
    amounts_recorded: true,
    lot: [leg({})],
    bid: [leg({ asset: USDC, kind: 'd_token', usd: '30.00' })],
    lot_usd: '33.00',
    lot_usd_lower_bound: false,
    bid_usd: '30.00',
    bid_usd_lower_bound: false,
    filler_profit_usd: '3.00',
    usd_reason: null,
    ...over,
  };
}

function respond(
  pages: Record<string, { fills: Liquidation[]; next?: string }>,
) {
  vi.mocked(apiGet).mockImplementation(
    async (
      path: string,
      params?: Record<string, string | number | undefined>,
    ) => {
      if (path.endsWith('/liquidations')) {
        const p = pages[String(params?.cursor ?? '')];
        return {
          data: {
            pool: POOL,
            valued_at: 'event_time',
            price_max_age_seconds: 86400,
            liquidations: p.fills,
          },
          pagination: p.next ? { next: p.next } : undefined,
        };
      }
      if (path.endsWith('/sac-wrappers')) return { data: {} };
      return {
        data: {
          reserves: [
            { asset: XLM, decimals: 7 },
            { asset: USDC, decimals: 7 },
          ],
        },
      };
    },
  );
}

function wrap(ui: React.ReactNode) {
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  });
  return render(
    <QueryClientProvider client={client}>{ui}</QueryClientProvider>,
  );
}

beforeEach(() => {
  vi.mocked(apiGet).mockReset();
});

describe('LiquidationsPanel', () => {
  it('renders an unvalued leg as a dash carrying the reason in words', async () => {
    respond({
      '': {
        fills: [
          fill({
            bid: [
              leg({
                asset: USDC,
                kind: 'd_token',
                underlying: null,
                price_usd: null,
                price_at: null,
                usd: null,
                reason: 'rate_at_fill_unrecorded',
              }),
            ],
            bid_usd: null,
            filler_profit_usd: null,
          }),
        ],
      },
    });
    wrap(<LiquidationsPanel pool={POOL} />);
    const row = await screen.findByTestId('liq-bid-0');
    const dash = within(row).getByText('—');
    expect(dash.getAttribute('title')).toMatch(
      /exchange rate at the fill was not recorded/,
    );
    expect(within(row).getByText(/dTokens/)).toBeTruthy();
  });

  it('marks a lower-bound side total with ≥', async () => {
    respond({
      '': {
        fills: [
          fill({
            lot: [
              leg({}),
              leg({ asset: USDC, usd: null, reason: 'price_thin_market' }),
            ],
            lot_usd: '33.00',
            lot_usd_lower_bound: true,
            filler_profit_usd: null,
          }),
        ],
      },
    });
    wrap(<LiquidationsPanel pool={POOL} />);
    const row = await screen.findByTestId('liq-lot-0');
    expect(row.textContent).toContain('≥');
    expect(within(row).getByTitle(/At least \$33\.00/)).toBeTruthy();
  });

  it('says a Blend V1 fill recorded no amounts', async () => {
    respond({
      '': {
        fills: [
          fill({
            amounts_recorded: false,
            lot: [],
            bid: [],
            lot_usd: null,
            bid_usd: null,
            filler_profit_usd: null,
            usd_reason: 'amounts_not_emitted',
          }),
        ],
      },
    });
    wrap(<LiquidationsPanel pool={POOL} />);
    const cell = await screen.findByText('Amounts not recorded');
    expect(cell.getAttribute('title')).toMatch(
      /Blend V1 pool does not record amounts/,
    );
    expect(screen.queryByText(/Debt repaid per day/)).toBeNull();
  });

  it('shows the empty state', async () => {
    respond({ '': { fills: [] } });
    wrap(<LiquidationsPanel pool={POOL} />);
    expect(
      await screen.findByText(/No liquidation fill recorded/),
    ).toBeTruthy();
  });

  it('loads more with the cursor and recounts the per-day chart', async () => {
    respond({
      '': { fills: [fill({})], next: 'c1' },
      c1: {
        fills: [
          fill({
            time: '2026-07-02T10:00:00Z',
            tx_hash: 'b'.repeat(64),
            bid_usd: '1250.50',
          }),
        ],
      },
    });
    wrap(<LiquidationsPanel pool={POOL} />);
    expect(
      await screen.findByText('Debt repaid per day — the 1 fill loaded'),
    ).toBeTruthy();
    fireEvent.click(screen.getByText('Load more'));
    expect(
      await screen.findByText('Debt repaid per day — the 2 fills loaded'),
    ).toBeTruthy();
    expect(vi.mocked(apiGet)).toHaveBeenCalledWith(
      `/v1/lending/pools/${POOL}/liquidations`,
      expect.objectContaining({ cursor: 'c1' }),
    );
    expect(screen.getByText('2026-07-02')).toBeTruthy();
    expect(screen.getByText('$1,251')).toBeTruthy();
    expect(screen.queryByText('Load more')).toBeNull();
  });
});
