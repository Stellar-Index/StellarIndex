import { describe, it, expect, vi, beforeEach } from 'vitest';
import { render, screen } from '@testing-library/react';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';

import { AccountsAnalytics } from './AccountsAnalytics';

const statsFixture = {
  data: {
    totals: {
      accounts: 9_930_118,
      trustlines: 19_400_000,
      trustline_holding_accounts: 3_100_000,
      xlm_held_stroops: '1049598291112345678',
    },
    balances: {
      avg_stroops: '1056980000',
      median_stroops: '50000000',
      p90_stroops: '3200000000',
      p99_stroops: '91000000000',
    },
    concentration: { top100_xlm_stroops: '512345678901234567', top100_share_pct: 48.82 },
    wealth_histogram: [
      { bucket: -1, accounts: 4_100_000, xlm_stroops: '9876543210' },
      { bucket: 3, accounts: 120_000, xlm_stroops: '55500000000000000' },
    ],
    trustline_histogram: [
      { bucket: '0', accounts: 6_830_118 },
      { bucket: '1', accounts: 1_400_000 },
      { bucket: '2-5', accounts: 1_200_000 },
    ],
    top_held_assets: [
      { asset: 'USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN', holders: 619_460 },
    ],
    computed_at: '2026-08-08T19:30:00Z',
  },
  as_of: '2026-08-08T19:45:00Z',
  flags: {},
};

vi.mock('@/api/client', async (importOriginal) => {
  const mod = await importOriginal<typeof import('@/api/client')>();
  return {
    ...mod,
    apiGet: vi.fn(async () => statsFixture),
  };
});

function renderWithQuery(ui: React.ReactElement) {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(<QueryClientProvider client={qc}>{ui}</QueryClientProvider>);
}

describe('AccountsAnalytics', () => {
  beforeEach(() => vi.clearAllMocks());

  it('renders totals, distribution charts, and the most-held board', async () => {
    renderWithQuery(<AccountsAnalytics />);

    expect(await screen.findByText('Funded accounts')).toBeInTheDocument();
    expect(screen.getByText('9.93M')).toBeInTheDocument();
    // Concentration stat derived from exact stroops figures.
    expect(screen.getByText('48.82%')).toBeInTheDocument();
    // Wealth bands render as XLM ranges, not raw bucket numbers.
    expect(screen.getByText('< 1 XLM')).toBeInTheDocument();
    expect(screen.getByText('1K–10K XLM')).toBeInTheDocument();
    // Trustline bands include the derived zero band.
    expect(screen.getByText('no trustlines')).toBeInTheDocument();
    // Charts carry accessible labels (semantics, not styling).
    expect(screen.getByLabelText('Accounts by XLM balance band')).toBeInTheDocument();
    expect(screen.getByLabelText('Accounts by trustline count band')).toBeInTheDocument();
    // Most-held board present with the holder count.
    expect(screen.getByText('Most held assets')).toBeInTheDocument();
    expect(screen.getByText('619.46K')).toBeInTheDocument();
  });
});
