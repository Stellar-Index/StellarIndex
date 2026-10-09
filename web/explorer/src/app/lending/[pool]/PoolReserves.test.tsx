import { describe, it, expect, vi, beforeEach } from 'vitest';
import { render, screen } from '@testing-library/react';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';

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

function reserve(asset: string, suppliedUsd: string | null) {
  return {
    asset,
    decimals: 7,
    supplied: '10000000000',
    borrowed: '0',
    supplied_usd: suppliedUsd,
    borrowed_usd: suppliedUsd == null ? null : '0',
    utilization_pct: 0,
    borrow_apr: null,
    supply_apr: null,
  };
}

function renderIt() {
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  });
  return render(
    <QueryClientProvider client={client}>
      <PoolReserves pool={POOL} />
    </QueryClientProvider>,
  );
}

beforeEach(() => {
  vi.mocked(apiGet).mockReset();
});

describe('PoolReserves', () => {
  it('marks a partial tvl_usd as a lower bound and names the unpriced reserve', async () => {
    vi.mocked(apiGet).mockResolvedValue({
      data: {
        pool: POOL,
        tvl_usd: '1000',
        lower_bound: true,
        reserves: [reserve('native', '1000'), reserve('USDX-GABC', null)],
      },
    });
    renderIt();
    const line = await screen.findByText(/Pool TVL:/);
    expect(line.textContent).toMatch(
      /^Pool TVL: ≥ \$1,000 \(at least this — .*excludes unpriced USDX\)$/,
    );
    expect(screen.getByText('≥ $1K')).toBeInTheDocument();
  });

  it('renders a fully priced tvl_usd as a plain total', async () => {
    vi.mocked(apiGet).mockResolvedValue({
      data: {
        pool: POOL,
        tvl_usd: '1000',
        lower_bound: false,
        reserves: [reserve('native', '1000')],
      },
    });
    renderIt();
    const line = await screen.findByText(/Pool TVL:/);
    expect(line.textContent).toBe(
      'Pool TVL: $1,000 (Σ supplied across priced reserves)',
    );
    expect(screen.queryByText(/^≥/)).not.toBeInTheDocument();
  });

  it('shows a tvl_usd above 2^53 digit for digit', async () => {
    vi.mocked(apiGet).mockResolvedValue({
      data: {
        pool: POOL,
        tvl_usd: '9007199254740993',
        lower_bound: false,
        reserves: [reserve('native', '9007199254740993')],
      },
    });
    renderIt();
    const line = await screen.findByText(/Pool TVL:/);
    expect(line.textContent).toContain('$9,007,199,254,740,993');
    expect(
      screen.getAllByText('$9,007,199,254,740,993').length,
    ).toBeGreaterThan(0);
  });
});
