import { describe, it, expect, vi } from 'vitest';
import { render, screen } from '@testing-library/react';

vi.mock('@/lib/buildFetch', async () => {
  const actual = await vi.importActual<typeof import('@/lib/buildFetch')>('@/lib/buildFetch');
  return { ...actual, buildFetchData: vi.fn(), failBuild: vi.fn() };
});

// PoolReserves is a 'use client' TanStack-Query component; the bake-time
// assertions below only care about the stat tiles above it.
vi.mock('./PoolReserves', () => ({ PoolReserves: () => <div /> }));

import { buildFetchData } from '@/lib/buildFetch';
import LendingPoolPage from './page';

// A curated Blend contract that carries a label but never appears in the
// auction listing.
const BACKSTOP = 'CAQQR5SWBXKIGZKPBZDH3KM5GQ5GUTPKB7JAFCINLZBC5WXPJKRG3IM7';
const POOL_1 = 'CAJJZSGMMM3PD7N33TAPHGBUGTB43OC73HVIK2L2G6BNGGGYOSSYBXBD';

async function renderPool(pool: string) {
  render(await LendingPoolPage({ params: Promise.resolve({ pool }) }));
}

// Frontend-honesty sweep: /v1/lending/pools serves "200 + empty array"
// when no LendingReader is wired (internal/api/v1/lending.go), and
// buildFetchData returns null on a CI-stub build. Both used to collapse
// into `data?.auctions_total ?? 0`, baking "Auctions (total): 0" into the
// static export for pools with thousands of real auctions. A listing that
// answered but omits this pool IS a real zero and must stay "0".
describe('LendingPoolPage auction stats', () => {
  it('renders — and says so, not 0, when the listing carries no rows', async () => {
    vi.mocked(buildFetchData).mockResolvedValue(null);
    await renderPool(POOL_1);

    expect(screen.getByText(/Auction statistics are unavailable for this build/)).toBeInTheDocument();
    // All three tiles read as unknown; none of them asserts a count.
    const tiles = ['Auctions (24h)', 'Auctions (total)', 'Unique users (30d)'];
    for (const label of tiles) {
      const value = screen.getByText(label).nextElementSibling;
      expect(value?.textContent).toBe('—');
    }
  });

  it('renders a genuine 0 when the listing answered but omits this pool', async () => {
    vi.mocked(buildFetchData).mockResolvedValue([
      {
        protocol: 'blend',
        pool: POOL_1,
        auctions_24h: 3,
        auctions_total: 40,
        unique_users_30d: 12,
        last_seen: '2026-08-12T00:00:00Z',
      },
    ]);
    await renderPool(BACKSTOP);

    expect(
      screen.queryByText(/Auction statistics are unavailable for this build/),
    ).not.toBeInTheDocument();
    const value = screen.getByText('Auctions (total)').nextElementSibling;
    expect(value?.textContent).toBe('0');
  });

  it('renders the served counts when the pool is listed', async () => {
    vi.mocked(buildFetchData).mockResolvedValue([
      {
        protocol: 'blend',
        pool: POOL_1,
        auctions_24h: 29,
        auctions_total: 7430,
        unique_users_30d: 10946,
        last_seen: '2026-08-12T00:00:00Z',
      },
    ]);
    await renderPool(POOL_1);

    expect(screen.getByText('7,430')).toBeInTheDocument();
    expect(screen.getByText('10,946')).toBeInTheDocument();
    expect(
      screen.queryByText(/Auction statistics are unavailable for this build/),
    ).not.toBeInTheDocument();
  });
});
