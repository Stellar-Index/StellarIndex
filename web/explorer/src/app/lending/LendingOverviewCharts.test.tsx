import { describe, it, expect, vi, beforeEach } from 'vitest';
import { render, screen, waitFor } from '@testing-library/react';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';

import {
  LendingOverviewCharts,
  protocolRows,
  topPoolsWithTail,
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

describe('protocolRows', () => {
  it('keeps exact inputs for hover and marks a lower bound', () => {
    const [r] = protocolRows([proto({ lower_bound: true })]);
    expect(r.supplied?.display).toMatch(/^≥\$1\.2/);
    expect(r.supplied?.exact).toContain('At least $1,234,567.89');
    expect(r.lowerBound).toBe(true);
    expect(r.color).toBe('var(--color-chart-1)');
  });
  it('scales borrowed within the protocol and shares across protocols', () => {
    const rows = protocolRows([
      proto({ total_supplied_usd: '300', total_borrowed_usd: '75' }),
      proto({
        protocol: 'blnt',
        total_supplied_usd: '100',
        total_borrowed_usd: '50',
      }),
    ]);
    expect(rows.map((r) => r.utilPct)).toEqual([25, 50]);
    expect(rows.map((r) => r.sharePct)).toEqual([75, 25]);
  });
  it('drops a protocol with nothing priced', () => {
    expect(
      protocolRows([
        proto({ total_supplied_usd: null, total_borrowed_usd: null }),
      ]),
    ).toEqual([]);
  });
});

describe('topPoolsWithTail', () => {
  it('groups the long tail into one exact row', () => {
    const { items } = tvlBarItems(
      ['1000', '500', '0.10', '0.20'].map((v, i) =>
        pool({ pool: `C${i}`, summary: { tvl_usd: v, lower_bound: false } }),
      ),
    );
    const shown = topPoolsWithTail(items, 2);
    expect(shown).toHaveLength(3);
    expect(shown[2]).toMatchObject({ label: '2 more pools' });
    expect(shown[2].title).toMatch(/^\$0\.30 · /);
  });
  it('never draws a lower bound of zero', () => {
    const { items, unpriced } = tvlBarItems([
      pool({ summary: { tvl_usd: '0', lower_bound: true } }),
    ]);
    expect(items).toEqual([]);
    expect(unpriced).toBe(1);
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
