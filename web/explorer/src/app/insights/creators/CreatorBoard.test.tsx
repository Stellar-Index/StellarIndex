import { describe, it, expect, vi, beforeEach } from 'vitest';
import { render, screen } from '@testing-library/react';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';

import { CreatorBoard } from './CreatorBoard';

// The fixture's coverage span deliberately does NOT start at genesis:
// it opens at ledger 33,000,001. The page must say so rather than
// implying the counts cover the whole chain.
const creatorsFixture = {
  data: {
    creators: [
      {
        rank: 1,
        account: 'GBMUZ7DCFWJ47CI2FGFR4NIVSZNPPZENJJWNG7THSRWQWFZVNUNZJTR4',
        accounts_created: 108_730,
        funded_stroops: '5290690000000',
        live_accounts: 4_346,
        live_stroops: '71721284463',
        first_ledger: 33_000_001,
        last_ledger: 50_999_236,
        first_created_at: '2024-03-18T04:11:02Z',
        last_created_at: '2024-05-02T22:07:44Z',
      },
      {
        // Sponsored-only creator: funded nothing of its own (CAP-33).
        rank: 2,
        account: 'GCZGSFPITKVJPJERJIVLCQK5YIHYTDXCY45ZHU3IRCUC53SXSCAL44JV',
        accounts_created: 52_297,
        funded_stroops: '0',
        live_accounts: 52_094,
        live_stroops: '107391888628',
        first_ledger: 33_000_023,
        last_ledger: 50_999_990,
        first_created_at: '2024-03-18T05:00:00Z',
        last_created_at: '2024-05-02T23:00:00Z',
      },
    ],
    totals: {
      creators: 13_119,
      accounts_created: 359_328,
      live_accounts: 71_000,
    },
    coverage: {
      from_ledger: 33_000_001,
      thru_ledger: 50_999_990,
      from_time: '2024-03-18T04:11:02Z',
      thru_time: '2024-05-02T23:00:00Z',
    },
    computed_at: '2026-09-05T09:30:00Z',
  },
  as_of: '2026-09-05T09:45:00Z',
  flags: {},
};

vi.mock('@/api/client', async (importOriginal) => {
  const mod = await importOriginal<typeof import('@/api/client')>();
  return {
    ...mod,
    apiGet: vi.fn(async () => creatorsFixture),
  };
});

function renderWithQuery(ui: React.ReactElement) {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(<QueryClientProvider client={qc}>{ui}</QueryClientProvider>);
}

describe('CreatorBoard', () => {
  beforeEach(() => vi.clearAllMocks());

  it('renders the board with immutable and point-in-time columns kept apart', async () => {
    renderWithQuery(<CreatorBoard />);

    expect(await screen.findByText('Creators')).toBeInTheDocument();
    expect(screen.getByText('13.12K')).toBeInTheDocument();
    // Created vs live are distinct columns, never summed together.
    expect(screen.getByText('108,730')).toBeInTheDocument();
    expect(screen.getByText('4,346')).toBeInTheDocument();
    // Survival is derived from the two exact counts beside it.
    expect(screen.getByText('4.0%')).toBeInTheDocument();
    // Stroops render as XLM through the BigInt path, never Number().
    expect(screen.getByText('529,069')).toBeInTheDocument();
  });

  it('states the covered ledger span instead of implying the whole chain', async () => {
    renderWithQuery(<CreatorBoard />);

    // The exact span the rollup aggregated, both bounds, rendered as text
    // a reader can act on — not a genesis floor the data does not back.
    const strip = await screen.findByText(/the span the rollup/i);
    expect(strip.textContent).toContain('33,000,001');
    expect(strip.textContent).toContain('50,999,990');
    expect(strip.textContent).not.toContain('whole chain claim');
  });

  it('renders a sponsored creator’s zero funding as a real figure', async () => {
    renderWithQuery(<CreatorBoard />);

    expect(await screen.findByText('52,297')).toBeInTheDocument();
    // 0 stroops must reach the table as 0 XLM, not as an em dash or a gap.
    expect(screen.getByText('0')).toBeInTheDocument();
    expect(screen.getByText(/CAP-33/)).toBeInTheDocument();
  });

  it('falls back to a warming message rather than an empty board', async () => {
    const mod = await import('@/api/client');
    vi.mocked(mod.apiGet).mockRejectedValueOnce(new Error('503'));

    renderWithQuery(<CreatorBoard />);

    expect(await screen.findByText(/warming/i)).toBeInTheDocument();
  });
});
