import { describe, it, expect, vi, beforeEach } from 'vitest';
import { render, screen, waitFor } from '@testing-library/react';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';

import {
  LendingOverviewCharts,
  protocolPairRows,
  tvlBarItems,
} from './LendingOverviewCharts';

vi.mock('@/api/client', async () => {
  const actual =
    await vi.importActual<typeof import('@/api/client')>('@/api/client');
  return { ...actual, apiGet: vi.fn() };
});

import { apiGet } from '@/api/client';

const BLEND = 'CAJJZSGMMM3PD7N33TAPHGBUGTB43OC73HVIK2L2G6BNGGGYOSSYBXBD';

const proto = (over: Record<string, unknown>) =>
  ({
    protocol: 'blend',
    total_supplied_usd: '1234567.89',
    total_borrowed_usd: '500000.01',
    lower_bound: false,
    ...over,
  }) as never;

const pool = (over: Record<string, unknown>) =>
  ({
    pool: BLEND,
    protocol: 'blend',
    summary: { tvl_usd: '2500000.5', lower_bound: false },
    ...over,
  }) as never;

function renderIt() {
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  });
  return render(
    <QueryClientProvider client={client}>
      <LendingOverviewCharts />
    </QueryClientProvider>,
  );
}

beforeEach(() => {
  vi.mocked(apiGet).mockReset();
});

describe('protocolPairRows', () => {
  it('keeps exact inputs for hover and marks a lower bound', () => {
    const [r] = protocolPairRows([proto({ lower_bound: true })]);
    expect(r.aDisplay).toMatch(/^≥\$1\.2/);
    expect(r.title).toContain('At least $1,234,567.89');
    expect(r.aHatch).toBe(true);
    expect(r.color).toBe('var(--color-chart-1)');
  });
  it('drops a protocol with nothing priced', () => {
    expect(
      protocolPairRows([
        proto({ total_supplied_usd: null, total_borrowed_usd: null }),
      ]),
    ).toEqual([]);
  });
});

describe('tvlBarItems', () => {
  it('ranks priced pools, colours by protocol and counts unpriced ones', () => {
    const { items, unpriced } = tvlBarItems([
      pool({}),
      pool({
        pool: 'CBLNT',
        protocol: 'blnt',
        summary: { tvl_usd: '9000000', lower_bound: true },
      }),
      pool({ pool: 'CNONE', summary: { tvl_usd: null } }),
      pool({ pool: 'CPEND', summary: null, summary_pending: true }),
    ]);
    expect(items.map((i) => i.id)).toEqual(['CBLNT', BLEND]);
    expect(items[0]).toMatchObject({
      hatchTail: true,
      color: 'var(--color-chart-3)',
    });
    expect(items[0].display).toMatch(/^≥/);
    expect(items[1].title).toContain('$2,500,000.50');
    expect(unpriced).toBe(1);
  });
});

describe('LendingOverviewCharts', () => {
  it('renders both charts and the not-priced note', async () => {
    vi.mocked(apiGet).mockImplementation(async (path: string) =>
      path === '/v1/lending/protocols'
        ? { data: [proto({}), proto({ protocol: 'blnt' })] }
        : {
            data: [
              pool({}),
              pool({ pool: 'CNONE', summary: { tvl_usd: null } }),
            ],
          },
    );
    renderIt();
    await waitFor(() =>
      expect(screen.getByLabelText(/TVL per pool/)).toBeInTheDocument(),
    );
    expect(
      screen.getByLabelText(/Supplied and borrowed USD per protocol/),
    ).toBeInTheDocument();
    expect(
      screen.getByText(/1 pool not priced, not shown/),
    ).toBeInTheDocument();
  });

  it('shows error states when the reads fail', async () => {
    vi.mocked(apiGet).mockRejectedValue(new Error('boom'));
    renderIt();
    await waitFor(() => expect(screen.getAllByRole('alert')).toHaveLength(2));
  });
});
