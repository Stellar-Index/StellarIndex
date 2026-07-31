import { describe, it, expect, vi, beforeEach } from 'vitest';
import { render, screen, waitFor } from '@testing-library/react';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';

import { LendingPoolsTable } from './LendingPoolsTable';

vi.mock('@/api/client', async () => {
  const actual = await vi.importActual<typeof import('@/api/client')>('@/api/client');
  return { ...actual, apiGet: vi.fn() };
});

import { apiGet } from '@/api/client';

function renderIt() {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(
    <QueryClientProvider client={client}>
      <LendingPoolsTable />
    </QueryClientProvider>,
  );
}

beforeEach(() => {
  vi.mocked(apiGet).mockReset();
});

const POOL = 'CAJJZSGMMM3PD7N33TAPHGBUGTB43OC73HVIK2L2G6BNGGGYOSSYBXBD';

describe('LendingPoolsTable', () => {
  it('renders an explicit unavailable state on API failure — never the fabricated "no pools" empty state', async () => {
    vi.mocked(apiGet).mockRejectedValue(new Error('boom'));
    renderIt();
    await waitFor(() =>
      expect(screen.getByText(/The pools list is unavailable right now/)).toBeInTheDocument(),
    );
    expect(screen.queryByText(/No Blend pools have emitted auction events yet/)).toBeNull();
  });

  it('keeps the empty-state copy for a served-and-empty pools list', async () => {
    vi.mocked(apiGet).mockResolvedValue({ data: [] });
    renderIt();
    await waitFor(() =>
      expect(screen.getByText(/No Blend pools have emitted auction events yet/)).toBeInTheDocument(),
    );
  });

  it('computes utilization over priced reserves only — unpriced reserves are excluded, not counted as $0', async () => {
    vi.mocked(apiGet).mockImplementation(async (path: unknown) => {
      if (path === '/v1/lending/pools') {
        return { data: [{ pool: POOL, protocol: 'blend', auctions_total: 0, auctions_24h: 0 }] };
      }
      // One priced reserve (util 50%) + one wholly unpriced reserve. The
      // old coerced-zero math still produced 50% here, but the APR average
      // used to fold the unpriced reserve's APR in at weight 0 and absent
      // APRs in at 0% — the basis must be priced reserves only.
      return {
        data: {
          tvl_usd: '150',
          reserves: [
            { supplied_usd: '100', borrowed_usd: '50', supply_apr: 0.04, borrow_apr: 0.08 },
            { supply_apr: 0.99 },
          ],
        },
      };
    });
    renderIt();
    await waitFor(() => expect(screen.getByText('50.0%')).toBeInTheDocument());
    // Weighted APRs come from the single priced reserve alone.
    expect(screen.getByText('4.00%')).toBeInTheDocument();
    expect(screen.getByText('8.00%')).toBeInTheDocument();
    // The basis is labelled (util + both APR cells carry the same note).
    expect(screen.getAllByTitle('Priced reserves only (1 of 2)').length).toBeGreaterThan(0);
  });

  it('renders "—" when no reserve is priced instead of a fabricated 0-based percentage', async () => {
    vi.mocked(apiGet).mockImplementation(async (path: unknown) => {
      if (path === '/v1/lending/pools') {
        return { data: [{ pool: POOL, protocol: 'blend', auctions_total: 0, auctions_24h: 0 }] };
      }
      return { data: { reserves: [{ supply_apr: 0.05 }, {}] } };
    });
    renderIt();
    await waitFor(() => expect(screen.getByText(/Pool #1/)).toBeInTheDocument());
    // TVL, utilization, and both APRs all fall back to "—".
    await waitFor(() => expect(screen.getAllByText('—').length).toBeGreaterThanOrEqual(4));
    expect(screen.queryByText(/%$/)).toBeNull();
  });
});
