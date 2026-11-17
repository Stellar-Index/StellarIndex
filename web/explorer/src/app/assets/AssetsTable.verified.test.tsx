import { describe, it, expect, vi } from 'vitest';
import { render, screen } from '@testing-library/react';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';

import { AssetsTable } from './AssetsTable';
import type { Coin } from '@/api/hooks';

const REAL = `USDC-G${'A'.repeat(55)}`;
const FAKE = `USDC-G${'B'.repeat(55)}`;

const state = vi.hoisted(() => ({ assets: [] as unknown[] }));

vi.mock('next/navigation', () => ({
  useRouter: () => ({ push: vi.fn() }),
  useSearchParams: () => new URLSearchParams(''),
}));

vi.mock('@/api/hooks', async () => {
  const actual =
    await vi.importActual<typeof import('@/api/hooks')>('@/api/hooks');
  return {
    ...actual,
    useAssets: () => ({
      data: { assets: state.assets, next_cursor: '' },
      isLoading: false,
      isError: false,
      error: null,
    }),
  };
});

// The listing slug falls back to the bare code, so a same-code asset from
// another issuer carries the verified row's slug but not its asset id.
const row = (asset_id: string) =>
  ({
    kind: 'stellar_asset',
    asset_id,
    code: 'USDC',
    slug: 'usdc',
    issuer: asset_id.split('-')[1],
    decimals: 7,
    sep1_status: 'not_applicable',
    first_seen_ledger: 1,
    last_seen_ledger: 1,
    observation_count: 1,
    volume_24h_usd: '10',
    unverified_ticker_collision: false,
  }) as unknown as Coin;

function renderTable() {
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  });
  return render(
    <QueryClientProvider client={client}>
      <AssetsTable verifiedSlugs={['usdc']} verifiedAssetIds={[REAL]} />
    </QueryClientProvider>,
  );
}

describe('AssetsTable verified badge', () => {
  it('badges the catalogued asset id', () => {
    state.assets = [row(REAL)];
    renderTable();
    expect(screen.getAllByLabelText('Verified currency')).toHaveLength(1);
  });

  it('does not badge a same-code asset from another issuer', () => {
    state.assets = [row(FAKE)];
    renderTable();
    expect(screen.getAllByText(/USDC/).length).toBeGreaterThan(0);
    expect(screen.queryByLabelText('Verified currency')).toBeNull();
  });
});
