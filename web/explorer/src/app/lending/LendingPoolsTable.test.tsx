import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { render, screen, waitFor } from '@testing-library/react';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';

import {
  LendingPoolsTable,
  PENDING_POLL_MAX,
  PENDING_POLL_MS,
} from './LendingPoolsTable';

vi.mock('@/api/client', async () => {
  const actual =
    await vi.importActual<typeof import('@/api/client')>('@/api/client');
  return { ...actual, apiGet: vi.fn() };
});

import { apiGet } from '@/api/client';

function renderIt() {
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  });
  return render(
    <QueryClientProvider client={client}>
      <LendingPoolsTable />
    </QueryClientProvider>,
  );
}

beforeEach(() => {
  vi.mocked(apiGet).mockReset();
  vi.useFakeTimers({ shouldAdvanceTime: true });
});

afterEach(() => {
  vi.useRealTimers();
});

const POOL = 'CAJJZSGMMM3PD7N33TAPHGBUGTB43OC73HVIK2L2G6BNGGGYOSSYBXBD';

const BLNT_POOL = 'CACSQWCMKG45TUHCQ3LXHOB7YEEJH4W376ZHQPXH2IZ3TE4NJHFZMCY7';

describe('LendingPoolsTable', () => {
  it('renders an explicit unavailable state on API failure — never the fabricated "no pools" empty state', async () => {
    vi.mocked(apiGet).mockRejectedValue(new Error('boom'));
    renderIt();
    await waitFor(() =>
      expect(
        screen.getByText(/The pools list is unavailable right now/),
      ).toBeInTheDocument(),
    );
    expect(
      screen.queryByText(/No lending pools have emitted auction events yet/),
    ).toBeNull();
  });

  it('keeps the empty-state copy for a served-and-empty pools list', async () => {
    vi.mocked(apiGet).mockResolvedValue({ data: [] });
    renderIt();
    await waitFor(() =>
      expect(
        screen.getByText(/No lending pools have emitted auction events yet/),
      ).toBeInTheDocument(),
    );
  });

  it('says the list is a lower bound when the server flags it truncated, and only then', async () => {
    vi.mocked(apiGet).mockResolvedValue({
      data: [{ pool: POOL, protocol: 'blend', auctions_total: 0 }],
      flags: { truncated: true },
    });
    const first = renderIt();
    await waitFor(() =>
      expect(screen.getAllByText(/Partial list/).length).toBeGreaterThan(0),
    );
    first.unmount();

    vi.mocked(apiGet).mockResolvedValue({
      data: [{ pool: POOL, protocol: 'blend', auctions_total: 0 }],
      flags: {},
    });
    renderIt();
    await waitFor(() => expect(screen.getByText('Pools (1)')).toBeTruthy());
    expect(screen.queryByText(/Partial list/)).toBeNull();
  });

  const row = (extra: Record<string, unknown>) => ({
    pool: POOL,
    protocol: 'blend',
    auctions_total: 0,
    auctions_24h: 0,
    ...extra,
  });

  it('renders the server summary, a partial TVL as a lower bound, and never calls /reserves', async () => {
    vi.mocked(apiGet).mockResolvedValue({
      data: [
        row({
          summary: {
            tvl_usd: '2500000.00',
            lower_bound: true,
            borrowed_usd: '1000000.00',
            borrowed_lower_bound: true,
            utilization_pct: 40,
            supply_apr: 0.04,
            borrow_apr: 0.08,
            priced_reserve_count: 1,
            reserve_count: 2,
          },
          summary_pending: false,
          summary_error: false,
        }),
      ],
    });
    renderIt();
    await waitFor(() => expect(screen.getByText('40.0%')).toBeInTheDocument());
    expect(screen.getByText('≥$2.50M')).toBeInTheDocument();
    expect(screen.getByText('4.00%')).toBeInTheDocument();
    expect(screen.getByText('8.00%')).toBeInTheDocument();
    expect(screen.getAllByTitle('Priced reserves only (1 of 2)').length).toBe(
      3,
    );
    expect(vi.mocked(apiGet)).toHaveBeenCalledTimes(1);
  });

  it('renders "—" for a summary with nothing priced instead of a fabricated 0-based percentage', async () => {
    vi.mocked(apiGet).mockResolvedValue({
      data: [
        row({
          summary: {
            tvl_usd: null,
            lower_bound: false,
            borrowed_usd: null,
            borrowed_lower_bound: false,
            utilization_pct: null,
            supply_apr: null,
            borrow_apr: null,
            priced_reserve_count: 0,
            reserve_count: 2,
          },
        }),
      ],
    });
    renderIt();
    await waitFor(() =>
      expect(screen.getByText(/Pool #1/)).toBeInTheDocument(),
    );
    expect(screen.getAllByText('—').length).toBeGreaterThanOrEqual(4);
    expect(screen.queryByText(/%$/)).toBeNull();
  });

  it('shows "unavailable" for an errored summary and does not call /reserves', async () => {
    vi.mocked(apiGet).mockResolvedValue({
      data: [
        row({ summary: null, summary_pending: false, summary_error: true }),
      ],
    });
    renderIt();
    await waitFor(() =>
      expect(screen.getAllByText('unavailable').length).toBe(4),
    );
    expect(vi.mocked(apiGet)).toHaveBeenCalledTimes(1);
  });

  it('a pending row fetches /reserves once, then refetches the list for its summary', async () => {
    let listCalls = 0;
    vi.mocked(apiGet).mockImplementation(async (path: unknown) => {
      if (path === '/v1/lending/pools') {
        listCalls += 1;
        return {
          data: [
            listCalls === 1
              ? row({ summary: null, summary_pending: true })
              : row({
                  summary: {
                    tvl_usd: '100.00',
                    lower_bound: false,
                    utilization_pct: 50,
                    supply_apr: 0.04,
                    borrow_apr: 0.08,
                    priced_reserve_count: 1,
                    reserve_count: 1,
                  },
                }),
          ],
        };
      }
      return { data: { reserves: [] } };
    });
    renderIt();
    await waitFor(() => expect(listCalls).toBe(1));
    await vi.advanceTimersByTimeAsync(PENDING_POLL_MS);
    await waitFor(() => expect(screen.getByText('50.0%')).toBeInTheDocument());
    const reserveCalls = vi
      .mocked(apiGet)
      .mock.calls.filter(([p]) => String(p).endsWith('/reserves'));
    expect(reserveCalls.length).toBe(1);
    expect(listCalls).toBe(2);
  });

  it('a pending row whose /reserves call failed still picks up the summary the build later produced', async () => {
    let listCalls = 0;
    vi.mocked(apiGet).mockImplementation(async (path: unknown) => {
      if (path === '/v1/lending/pools') {
        listCalls += 1;
        return {
          data: [
            listCalls < 3
              ? row({ summary: null, summary_pending: true })
              : row({
                  summary: {
                    tvl_usd: '100.00',
                    lower_bound: false,
                    utilization_pct: 25,
                    priced_reserve_count: 1,
                    reserve_count: 1,
                  },
                }),
          ],
        };
      }
      throw new Error('503');
    });
    renderIt();
    await waitFor(() =>
      expect(screen.getAllByText('unavailable').length).toBeGreaterThan(0),
    );
    await vi.advanceTimersByTimeAsync(PENDING_POLL_MS * 2);
    await waitFor(() => expect(screen.getByText('25.0%')).toBeInTheDocument());
  });

  it('stops polling a list that stays pending', async () => {
    let listCalls = 0;
    vi.mocked(apiGet).mockImplementation(async (path: unknown) => {
      if (path === '/v1/lending/pools') {
        listCalls += 1;
        return { data: [row({ summary: null, summary_pending: true })] };
      }
      return { data: { reserves: [] } };
    });
    renderIt();
    await waitFor(() => expect(listCalls).toBe(1));
    await vi.advanceTimersByTimeAsync(PENDING_POLL_MS * (PENDING_POLL_MAX + 5));
    expect(listCalls).toBe(PENDING_POLL_MAX);
  });

  it('labels a BLNT pool from the curated map, badges its protocol and links to its pool page', async () => {
    vi.mocked(apiGet).mockResolvedValue({
      data: [{ pool: BLNT_POOL, protocol: 'blnt', auctions_total: 0 }],
    });
    renderIt();
    await waitFor(() =>
      expect(screen.getByText('BLNT Fixed pool')).toBeInTheDocument(),
    );
    expect(screen.getByText('BLNT V3')).toBeInTheDocument();
    expect(screen.queryByText(/Blend/)).toBeNull();
    const link = screen.getAllByRole('link').find((a) => a.title === BLNT_POOL);
    expect(link).toHaveAttribute('href', `/lending/${BLNT_POOL}`);
  });

  it('never calls an uncurated non-Blend row a Blend pool', async () => {
    vi.mocked(apiGet).mockResolvedValue({
      data: [
        row({ pool: 'CUNCURATED000', protocol: 'blnt' }),
        row({ pool: 'CUNKNOWN000', protocol: 'mystery' }),
      ],
    });
    renderIt();
    await waitFor(() =>
      expect(screen.getByText('BLNT V3 pool')).toBeInTheDocument(),
    );
    expect(screen.getByText('Lending pool')).toBeInTheDocument();
    expect(screen.queryByText(/Blend/)).toBeNull();
  });
});
