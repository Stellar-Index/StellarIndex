import { describe, it, expect, vi, beforeEach } from 'vitest';
import { fireEvent, render, screen, within } from '@testing-library/react';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';

import { TopAccountsPanel } from './TopAccountsPanel';

vi.mock('@/api/client', async () => {
  const actual =
    await vi.importActual<typeof import('@/api/client')>('@/api/client');
  return { ...actual, apiGet: vi.fn() };
});

import { apiGet } from '@/api/client';

const POOL = 'CAJJZSGMMM3PD7N33TAPHGBUGTB43OC73HVIK2L2G6BNGGGYOSSYBXBD';
const USDC_SAC = 'CCW67TSZV3SSS2HXMBQ5JFGCKJNXKZM7UQUWUZPUTHXSTZLEO7SJMI75';
const EURC_SAC = 'CDTKPWPLOURQA2SGTKTUQOWRCBZEORB4BWBOMJ3D3ZTQQSGE5F6JBQLV';
const ACCOUNT = 'GAAZI4TCR3TY5OJHCTJC2A4QSY6CJWJH5IAJTGKIN2ER7LBNVKOCCWN7';

function row(over: Record<string, unknown> = {}) {
  return {
    rank: 1,
    account: ACCOUNT,
    asset: USDC_SAC,
    decimals: 7,
    tokens: '9800000000000',
    amount: '10290000000000',
    value_usd: '1029000.00',
    usd_reason: null,
    share_of_reserve: '0.412000',
    last_modified_ledger: 63339870,
    ...over,
  };
}

function payload(side: string, over: Record<string, unknown> = {}) {
  return {
    data: {
      pool: POOL,
      protocol: 'blend',
      side,
      asset: null,
      ranking: 'value_usd',
      accounts: 412,
      lower_bound: false,
      excluded: [],
      rows: [row()],
      as_of_ledger: 63340102,
      note: '',
      ...over,
    },
  };
}

function mockApi(top: (q: Record<string, unknown>) => unknown) {
  vi.mocked(apiGet).mockImplementation(async (path: string, q?: unknown) => {
    if (path === '/v1/sac-wrappers')
      return {
        data: {
          [USDC_SAC]:
            'USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN',
          [EURC_SAC]:
            'EURC-GDHU6WRG4IEQXM5NZ4BMPKOXHW76MZM4Y2IEMFDVXBSDP6SJY4ITNPP2',
        },
      };
    return top((q ?? {}) as Record<string, unknown>);
  });
}

function renderIt() {
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  });
  return render(
    <QueryClientProvider client={client}>
      <TopAccountsPanel pool={POOL} />
    </QueryClientProvider>,
  );
}

beforeEach(() => {
  vi.mocked(apiGet).mockReset();
});

describe('TopAccountsPanel', () => {
  it('renders ranked rows with USD, readable amount and the share bar', async () => {
    mockApi((q) => payload(q.side as string));
    renderIt();
    expect(await screen.findByText('$1.03M')).toBeInTheDocument();
    expect(screen.getByText('412')).toBeInTheDocument();
    expect(screen.getByText(/accounts supplying/)).toBeInTheDocument();
    expect(screen.getByText('1.03M')).toBeInTheDocument();
    expect(screen.getByText('41.20%')).toBeInTheDocument();
    expect(screen.getByRole('meter')).toHaveAttribute('aria-valuenow', '41.2');
    const link = screen.getByTitle(ACCOUNT);
    expect(link).toHaveAttribute('href', `/accounts/${ACCOUNT}/`);
    expect(screen.queryByText(USDC_SAC)).not.toBeInTheDocument();
    expect(vi.mocked(apiGet)).toHaveBeenCalledWith(
      `/v1/lending/pools/${POOL}/top-accounts`,
      { side: 'supply', limit: 20 },
    );
  });

  it('switches to the borrow side', async () => {
    mockApi((q) =>
      payload(q.side as string, {
        accounts: q.side === 'borrow' ? 7 : 412,
      }),
    );
    renderIt();
    await screen.findByText('412');
    fireEvent.click(screen.getByRole('button', { name: 'Borrow' }));
    expect(await screen.findByText('7')).toBeInTheDocument();
    expect(screen.getByText(/accounts borrowing/)).toBeInTheDocument();
    expect(vi.mocked(apiGet)).toHaveBeenCalledWith(
      `/v1/lending/pools/${POOL}/top-accounts`,
      { side: 'borrow', limit: 20 },
    );
  });

  it('shows the withheld state with its reason when value_usd is null', async () => {
    mockApi((q) =>
      payload(q.side as string, {
        ranking: 'amount',
        asset: EURC_SAC,
        rows: [
          row({ asset: EURC_SAC, value_usd: null, usd_reason: 'unpriced' }),
        ],
      }),
    );
    renderIt();
    const dash = await screen.findByTitle('no reliable price');
    expect(dash.textContent).toBe('—');
    expect(screen.queryByText(/^\$/)).not.toBeInTheDocument();
  });

  it('marks a lower bound and names what it left out', async () => {
    mockApi((q) =>
      payload(q.side as string, {
        lower_bound: true,
        excluded: [
          { kind: 'supply', pool: POOL, asset: EURC_SAC, reason: 'unpriced' },
          {
            kind: 'supply',
            pool: POOL,
            asset: null,
            reason: 'positions_truncated',
          },
        ],
      }),
    );
    renderIt();
    expect(await screen.findByText('at least 412')).toBeInTheDocument();
    const note = screen.getByText(/partial:/);
    expect(note.textContent).toContain('EURC (no reliable price)');
    expect(note.textContent).toContain(
      'too many positions to rank in one read',
    );
  });

  it('words the reconciliation and rate reasons plainly on a partial ranking', async () => {
    mockApi((q) =>
      payload(q.side as string, {
        lower_bound: true,
        excluded: [
          {
            kind: 'supply',
            pool: POOL,
            asset: EURC_SAC,
            reason: 'positions_unreconciled',
          },
          {
            kind: 'supply',
            pool: POOL,
            asset: EURC_SAC,
            reason: 'rate_unread',
          },
        ],
      }),
    );
    renderIt();
    expect(await screen.findByText('at least 412')).toBeInTheDocument();
    const note = screen.getByText(/partial:/);
    expect(note.textContent).toContain(
      "EURC (the positions read did not add up to the reserve's total)",
    );
    expect(note.textContent).toContain(
      "EURC (the reserve's rate could not be read)",
    );
    expect(note.textContent).not.toContain('positions_unreconciled');
  });

  it('says why when no position can be ranked', async () => {
    mockApi((q) =>
      payload(q.side as string, {
        accounts: 3,
        rows: [],
        lower_bound: true,
        excluded: [
          {
            kind: 'supply',
            pool: POOL,
            asset: USDC_SAC,
            reason: 'positions_unreconciled',
          },
          {
            kind: 'supply',
            pool: POOL,
            asset: null,
            reason: 'positions_truncated',
          },
        ],
      }),
    );
    renderIt();
    expect(
      await screen.findByText('No rankable positions.'),
    ).toBeInTheDocument();
    const why = screen.getByText(/^Why:/);
    expect(why.textContent).toContain(
      "USDC (the positions read did not add up to the reserve's total)",
    );
    expect(why.textContent).toContain('too many positions to rank in one read');
    expect(screen.queryByText('No positions.')).not.toBeInTheDocument();
    expect(screen.queryByText(/accounts supplying/)).not.toBeInTheDocument();
    expect(screen.queryByRole('table')).not.toBeInTheDocument();
  });

  it('shows "no positions" for an empty ranking', async () => {
    mockApi((q) => payload(q.side as string, { accounts: 0, rows: [] }));
    renderIt();
    expect(await screen.findByText('No positions.')).toBeInTheDocument();
    expect(screen.queryByText(/No rankable/)).not.toBeInTheDocument();
    expect(screen.queryByRole('table')).not.toBeInTheDocument();
  });

  it('caps the share text at 100% like the meter', async () => {
    mockApi((q) =>
      payload(q.side as string, {
        rows: [row({ share_of_reserve: '1.250000' })],
      }),
    );
    renderIt();
    expect(await screen.findByText('100.00%')).toBeInTheDocument();
    expect(screen.queryByText('125.00%')).not.toBeInTheDocument();
  });

  it('notes archived positions unless the API note already does', async () => {
    mockApi((q) => payload(q.side as string));
    const { unmount } = renderIt();
    expect(
      await screen.findByText(/Positions include archived entries/),
    ).toBeInTheDocument();
    unmount();
    mockApi((q) =>
      payload(q.side as string, {
        note: 'Archived entries are included at their stored value.',
      }),
    );
    renderIt();
    await screen.findByText('412');
    expect(
      screen.queryByText(/Positions include archived entries/),
    ).not.toBeInTheDocument();
  });

  it('says the panel is unsupported on a 503, and offers a retry on other errors', async () => {
    mockApi(() => {
      throw new Error(
        '503 Service Unavailable on /v1/lending/pools/x/top-accounts',
      );
    });
    const { unmount } = renderIt();
    expect(
      await screen.findByText('Top accounts are not available for this pool.'),
    ).toBeInTheDocument();
    unmount();
    mockApi(() => {
      throw new Error(
        '500 Internal Server Error on /v1/lending/pools/x/top-accounts',
      );
    });
    renderIt();
    const msg = await screen.findByText(/unavailable right now/);
    expect(
      within(msg).getByRole('button', { name: 'Retry' }),
    ).toBeInTheDocument();
  });
});
