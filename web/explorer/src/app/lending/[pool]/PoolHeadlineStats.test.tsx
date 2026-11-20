import { describe, it, expect, vi, beforeEach } from 'vitest';
import { render, screen, waitFor } from '@testing-library/react';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';

import { PoolHeadlineStats } from './PoolHeadlineStats';
import { reserveAvailable } from './PoolReserves';

vi.mock('@/api/client', async () => {
  const actual =
    await vi.importActual<typeof import('@/api/client')>('@/api/client');
  return { ...actual, apiGet: vi.fn() };
});

import { apiGet } from '@/api/client';

const POOL = 'CAJJZSGMMM3PD7N33TAPHGBUGTB43OC73HVIK2L2G6BNGGGYOSSYBXBD';

function renderIt() {
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  });
  return render(
    <QueryClientProvider client={client}>
      <PoolHeadlineStats pool={POOL} />
    </QueryClientProvider>,
  );
}

beforeEach(() => {
  vi.mocked(apiGet).mockReset();
});

describe('reserveAvailable', () => {
  it('subtracts exactly above 2^53', () => {
    // 2^53 + 3 supplied, 2 borrowed: a float subtraction yields ...992.
    expect(
      reserveAvailable({ supplied: '9007199254740995', borrowed: '2' }).units,
    ).toBe('9007199254740993');
    expect(
      reserveAvailable({
        supplied: '1',
        borrowed: '0',
        supplied_usd: '9007199254740995.10',
        borrowed_usd: '2.05',
      }).usd,
    ).toBe('9007199254740993.05');
  });

  it('falls back to units when either side is unpriced', () => {
    expect(
      reserveAvailable({
        supplied: '10',
        borrowed: '4',
        supplied_usd: '10.00',
        borrowed_usd: null,
      }),
    ).toEqual({ usd: null, units: '6' });
  });
});

describe('PoolHeadlineStats', () => {
  it('leads with TVL and borrowed, lower bounds as ≥, a null USD as —', async () => {
    vi.mocked(apiGet).mockResolvedValue({
      data: [
        {
          pool: POOL,
          protocol: 'blend',
          summary: {
            tvl_usd: '2500000.00',
            lower_bound: true,
            borrowed_usd: null,
            borrowed_lower_bound: false,
            utilization_pct: 40,
            supply_apr: 0.04,
            borrow_apr: 0.08,
            priced_reserve_count: 1,
            reserve_count: 2,
          },
        },
      ],
    });
    renderIt();
    await waitFor(() => expect(screen.getByText('40.0%')).toBeInTheDocument());
    expect(screen.getByTitle(/At least \$2,500,000\.00/)).toHaveTextContent(
      '≥',
    );
    expect(screen.getByText('—')).toBeInTheDocument();
    expect(screen.queryByText('$0')).toBeNull();
    expect(screen.getByText('4.00%')).toBeInTheDocument();
  });

  it('shows an error state when the read fails', async () => {
    vi.mocked(apiGet).mockRejectedValue(new Error('boom'));
    renderIt();
    await waitFor(() =>
      expect(
        screen.getByText(/Pool totals are unavailable/),
      ).toBeInTheDocument(),
    );
  });
});
