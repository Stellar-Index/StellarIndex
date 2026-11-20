import { describe, it, expect, vi, beforeEach } from 'vitest';
import { render, screen, waitFor } from '@testing-library/react';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';

import { AccountLendingEarnings } from './AccountLendingEarnings';

vi.mock('@/api/client', async () => {
  const actual =
    await vi.importActual<typeof import('@/api/client')>('@/api/client');
  return { ...actual, apiGet: vi.fn() };
});

import { apiGet } from '@/api/client';

const G = 'GBRPYHIL2CI3FNQ4BXLFMNDLFJUNPU2HY3ZMFSHONUCEOASW7QC7OX2H';
const POOL = 'CACSQWCMKG45TUHCQ3LXHOB7YEEJH4W376ZHQPXH2IZ3TE4NJHFZMCY7';

const totals = (over: Record<string, unknown> = {}) => ({
  supplied_usd: '1500.00',
  borrowed_usd: '500.00',
  net_usd: null,
  net_usd_reason: 'inputs_partial',
  lower_bound: true,
  excluded: [],
  net_apr: null,
  net_apr_reason: 'inputs_partial',
  ...over,
});

const leg = (over: Record<string, unknown>) => ({
  protocol: 'blnt',
  pool: POOL,
  side: 'supply',
  tier: null,
  asset: 'USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN',
  decimals: 7,
  net_flow: '1',
  folded_tokens: null,
  onchain_tokens: null,
  current_value: '1',
  rate_as_of: null,
  accrued: '1',
  reconciled: true,
  reason: null,
  value_usd: '1500.00',
  accrued_usd: '12.50',
  usd_reason: null,
  ...over,
});

function renderIt() {
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  });
  return render(
    <QueryClientProvider client={client}>
      <AccountLendingEarnings id={G} />
    </QueryClientProvider>,
  );
}

beforeEach(() => {
  vi.mocked(apiGet).mockReset();
});

describe('AccountLendingEarnings', () => {
  it('values a BLNT pool, renders lower bounds as ≥, a null net as —, and names the unpriced leg', async () => {
    vi.mocked(apiGet).mockResolvedValue({
      data: {
        account: G,
        note: '',
        legs: [
          leg({}),
          leg({
            side: 'borrow',
            asset:
              'EURC-GDHU6WRG4IEQXM5NZ4BMPKOXHW76MZM4Y2IEMFDVXBSDP6SJY4ITNPP2',
            value_usd: null,
            accrued_usd: null,
            usd_reason: 'withheld',
          }),
        ],
        pools: [{ protocol: 'blnt', pool: POOL, ...totals() }],
        totals: totals(),
        emissions: [],
      },
    });
    renderIt();
    await waitFor(() =>
      expect(screen.getAllByText(/^BLNT/).length).toBeGreaterThan(0),
    );
    expect(screen.getAllByTitle(/At least \$1,500\.00/)[0]).toHaveTextContent(
      '≥',
    );
    expect(screen.queryByText('$0')).toBeNull();
    expect(
      screen.getAllByTitle('some legs are unpriced, so net is unknown')[0],
    ).toHaveTextContent('—');
    expect(screen.getByText(/Not valued:/)).toHaveTextContent(
      /EURC borrow .*price withheld/,
    );
    // Accrued: the withheld leg's USD is missing, so 12.50 is a floor.
    expect(screen.getByTitle(/At least \$12\.50/)).toBeInTheDocument();
  });

  it('shows an empty state for an account with no lending history', async () => {
    vi.mocked(apiGet).mockResolvedValue({
      data: {
        account: G,
        note: '',
        legs: [],
        pools: [],
        totals: totals({
          supplied_usd: '0',
          borrowed_usd: '0',
          lower_bound: false,
        }),
        emissions: [],
      },
    });
    renderIt();
    await waitFor(() =>
      expect(
        screen.getByText(/no Blend or BLNT lending history/),
      ).toBeInTheDocument(),
    );
  });

  it('shows an error state when the read fails', async () => {
    vi.mocked(apiGet).mockRejectedValue(new Error('boom'));
    renderIt();
    await waitFor(() =>
      expect(
        screen.getByText(/Lending positions are unavailable/),
      ).toBeInTheDocument(),
    );
  });
});
