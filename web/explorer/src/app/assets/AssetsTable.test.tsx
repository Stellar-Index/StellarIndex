import { describe, it, expect, vi } from 'vitest';
import { render, screen } from '@testing-library/react';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';

import { AssetsTable } from './AssetsTable';
import type { Coin } from '@/api/hooks';

// §3 scam-label surfacing — the assets directory table must badge a row
// whose issuer carries a curated directory scam flag (account_directory
// {malicious, unsafe}, e.g. the operator-reported scam AUD), and must NOT
// badge an unflagged asset. DISPLAY-ONLY: the badge is orthogonal to the
// price/verified columns.

const SCAM_AUD_ISSUER = 'GAIF52QZUPYCADXF7I7RNPMED7DT2B5JGPR7DEHCC5TPDPUJTMLGGAUD';

function coin(partial: Partial<Coin>): Coin {
  return {
    kind: 'stellar_asset',
    asset_id: 'x',
    code: 'X',
    slug: 'x',
    decimals: 7,
    sep1_status: 'not_applicable',
    first_seen_ledger: 1,
    last_seen_ledger: 1,
    observation_count: 1,
    ...partial,
  } as unknown as Coin;
}

const assets: Coin[] = [
  coin({
    asset_id: `AUD-${SCAM_AUD_ISSUER}`,
    code: 'AUD',
    slug: `AUD-${SCAM_AUD_ISSUER}`,
    issuer: SCAM_AUD_ISSUER,
    price_usd: '0.65',
    issuer_directory_tags: ['malicious', 'unsafe'],
    issuer_directory_domain: 'audrev-stellar.com',
    issuer_directory_name: 'Fake AUD',
  }),
  coin({
    asset_id: 'USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN',
    code: 'USDC',
    slug: 'usdc',
    price_usd: '1.0001',
  }),
];

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
      data: { assets, next_cursor: '' },
      isLoading: false,
      isError: false,
      error: null,
    }),
  };
});

function renderTable() {
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  });
  return render(
    <QueryClientProvider client={client}>
      <AssetsTable />
    </QueryClientProvider>,
  );
}

describe('AssetsTable directory scam badge', () => {
  it('badges the directory-flagged row and leaves the unflagged row unbadged', () => {
    renderTable();
    // Exactly one flagged badge — the scam AUD, not the normal USDC.
    const badges = screen.getAllByText(/Flagged/);
    expect(badges).toHaveLength(1);
    // Both rows still render their code + price — the badge is additive,
    // it does not replace or gate the row's data (display-only).
    expect(screen.getByText('AUD')).toBeInTheDocument();
    expect(screen.getByText('USDC')).toBeInTheDocument();
    // Attribution lands in the badge title (third-party, display-only).
    expect(badges[0]).toHaveAttribute(
      'title',
      expect.stringMatching(/stellar-expert community directory/),
    );
    expect(badges[0]).toHaveAttribute(
      'title',
      expect.stringMatching(/malicious, unsafe/),
    );
  });
});
