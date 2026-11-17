import { describe, it, expect, vi, beforeEach } from 'vitest';
import { fireEvent, render, screen } from '@testing-library/react';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';

import { AuctionsPanel } from './AuctionsPanel';
import { BackstopPanel } from './BackstopPanel';
import { ReserveHistory } from './ReserveHistory';

vi.mock('@/api/client', async () => {
  const actual =
    await vi.importActual<typeof import('@/api/client')>('@/api/client');
  return { ...actual, apiGet: vi.fn() };
});
vi.mock('@/lib/live/hooks', async (importOriginal) => ({
  ...(await importOriginal<typeof import('@/lib/live/hooks')>()),
  useLedgerFollow: () => {},
}));
vi.mock('./HistoryChart', () => ({
  HistoryChart: ({ label }: { label: string }) => <div>chart {label}</div>,
}));

import { apiGet } from '@/api/client';

const POOL = 'CAJJZSGMMM3PD7N33TAPHGBUGTB43OC73HVIK2L2G6BNGGGYOSSYBXBD';
const ASSET = 'CCW67TSZV3SSS2HXMBQ5JFGCKJNXKZM7UQUWUZPUTHXSTZLEO7SJMI75';

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

describe('BackstopPanel', () => {
  it('shows the reason when unavailable', async () => {
    vi.mocked(apiGet).mockResolvedValue({
      data: {
        pool: POOL,
        available: false,
        unavailable_reason: 'blend_v1_unsupported',
        tiers: [],
        config_decimals: 7,
      },
    });
    wrap(<BackstopPanel pool={POOL} />);
    expect(await screen.findByText(/Blend V1 pool/)).toBeTruthy();
  });

  it('labels BLNT tiers and renders null amounts as a dash', async () => {
    vi.mocked(apiGet).mockResolvedValue({
      data: {
        pool: POOL,
        available: true,
        take_rate: '5000000',
        config_decimals: 7,
        tiers: [
          {
            tier: 'FirstLoss',
            tokens: '50000000000000',
            shares: null,
            queued_shares: null,
            queued_pct: null,
            backstop_asset: 'Usdc',
            backstop_token: null,
            take_rate_weight: 40,
          },
          {
            tier: 'SecondLoss',
            tokens: null,
            shares: null,
            queued_shares: null,
            queued_pct: null,
            backstop_asset: null,
            backstop_token: null,
            take_rate_weight: null,
          },
        ],
      },
    });
    wrap(<BackstopPanel pool={POOL} />);
    expect(await screen.findByText('First-loss tier')).toBeTruthy();
    expect(screen.getByText('Second-loss tier')).toBeTruthy();
    expect(screen.getByText('5,000,000')).toBeTruthy();
    expect(screen.getByText('50%')).toBeTruthy();
    expect(screen.getAllByText('—').length).toBeGreaterThan(2);
  });
});

describe('AuctionsPanel', () => {
  it('fetches only after Show, then lists status and fills', async () => {
    vi.mocked(apiGet).mockImplementation(async (path: string) =>
      path.endsWith('/auctions')
        ? {
            data: {
              pool: POOL,
              auctions: [
                {
                  auction_type: 0,
                  auction_type_label: 'user_liquidation',
                  user: 'G'.padEnd(56, 'A'),
                  start_ledger: 5,
                  start_time: '2026-01-01T00:00:00Z',
                  tx_hash: 'a'.repeat(64),
                  percent: 50,
                  block: 1,
                  bid: {},
                  lot: {},
                  deleted: false,
                  fills: [
                    {
                      filler: 'G'.padEnd(56, 'B'),
                      fill_percent: '40',
                      ledger: 6,
                      time: '2026-01-01T00:01:00Z',
                      tx_hash: 'b'.repeat(64),
                    },
                  ],
                },
              ],
            },
            pagination: {},
          }
        : { data: { reserves: [] } },
    );
    wrap(<AuctionsPanel pool={POOL} />);
    expect(apiGet).not.toHaveBeenCalled();
    fireEvent.click(screen.getByText('Show auctions'));
    expect(await screen.findByText('Filled 40%')).toBeTruthy();
    fireEvent.click(screen.getByText('Fills (1)'));
    expect(screen.getByText(/filled/)).toBeTruthy();
  });

  it('says when the fill and delete read was truncated', async () => {
    vi.mocked(apiGet).mockResolvedValue({
      data: {
        pool: POOL,
        auctions: [],
        lifecycle_truncated: true,
        reserves: [],
      },
      pagination: {},
    });
    wrap(<AuctionsPanel pool={POOL} />);
    fireEvent.click(screen.getByText('Show auctions'));
    expect(await screen.findByText(/hit its cap/)).toBeTruthy();
  });

  it('shows the empty state', async () => {
    vi.mocked(apiGet).mockResolvedValue({
      data: { pool: POOL, auctions: [], reserves: [] },
      pagination: {},
    });
    wrap(<AuctionsPanel pool={POOL} />);
    fireEvent.click(screen.getByText('Show auctions'));
    expect(await screen.findByText(/No auctions recorded/)).toBeTruthy();
  });
});

describe('ReserveHistory', () => {
  it('shows the building state on a 503', async () => {
    vi.mocked(apiGet).mockImplementation(async () => {
      throw new Error('503 Service Unavailable on /x — history unavailable');
    });
    wrap(<ReserveHistory pool={POOL} asset={ASSET} />);
    expect(await screen.findByText(/still being built/)).toBeTruthy();
  });

  it('shows the stale flag and dropped points', async () => {
    vi.mocked(apiGet).mockResolvedValue({
      data: {
        decimals: 7,
        config_decimals: 7,
        from: '2026-01-01T00:00:00Z',
        to: '2026-01-10T00:00:00Z',
        dropped_points: 2,
        points: [],
      },
      flags: { stale: true },
    });
    wrap(<ReserveHistory pool={POOL} asset={ASSET} />);
    expect(await screen.findByText('Stale')).toBeTruthy();
    expect(screen.getByText(/2 points could not be decoded/)).toBeTruthy();
  });

  it('draws the chart, data-through note and config changes', async () => {
    vi.mocked(apiGet).mockResolvedValue({
      data: {
        decimals: 7,
        config_decimals: 7,
        from: '2026-01-01T00:00:00Z',
        to: '2026-01-10T00:00:00Z',
        history_through_time: '2026-01-09T12:00:00Z',
        before_first_state: true,
        points: [
          {
            t: '2026-01-01T00:00:00Z',
            removed: false,
            supplied: '10',
            borrowed: '5',
            utilization_pct: 50,
            borrow_apr: 0.06,
            supply_apr: null,
            collateral_factor: '7500000',
            liability_factor: null,
            supply_cap: null,
          },
          {
            t: '2026-01-05T00:00:00Z',
            removed: false,
            supplied: '10',
            borrowed: '5',
            utilization_pct: 50,
            borrow_apr: 0.07,
            supply_apr: null,
            collateral_factor: '8000000',
            liability_factor: null,
            supply_cap: null,
          },
        ],
      },
    });
    wrap(<ReserveHistory pool={POOL} asset={ASSET} />);
    expect(await screen.findByText('chart Borrow APR')).toBeTruthy();
    expect(screen.getByText(/Data through/)).toBeTruthy();
    expect(screen.getByText(/no recorded state before/)).toBeTruthy();
    expect(screen.getByText('75%')).toBeTruthy();
    expect(screen.getByText('80%')).toBeTruthy();
  });
});
