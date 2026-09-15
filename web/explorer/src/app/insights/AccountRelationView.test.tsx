import { describe, it, expect, beforeEach, vi } from 'vitest';
import { render, screen } from '@testing-library/react';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';

import { AccountRelationView } from './AccountRelationView';

const ACCOUNT = 'GDB3RSSWTUXO7MBTNMHUP3DRBIUR3QRV2CVFRAKMN4GM2B4QNGEUT6CU';
const CREATOR = 'GC5LF63GRVIT5ZXXCXLPI3RX2YXKJQFZVBSAO6AUELN3YIMSWPD6Z6FH';

const apiGet = vi.hoisted(() => vi.fn());
vi.mock('@/api/client', async (importOriginal) => {
  const mod = await importOriginal<typeof import('@/api/client')>();
  return { ...mod, apiGet };
});

vi.mock('@/components/charts/LineChart', () => ({
  LineChart: () => <div data-testid="line-chart" />,
}));
vi.mock('@/components/charts/DonutChart', () => ({
  DonutChart: () => <div data-testid="donut-chart" />,
}));

const coverage = {
  from_ledger: 3,
  thru_ledger: 64_428_050,
  from_time: '2015-09-30T17:15:54Z',
  thru_time: '2026-09-14T17:45:39Z',
  computed_at: '2026-09-14T17:57:59Z',
};

const graph = {
  data: {
    account: ACCOUNT,
    inbound: {
      created_by: {
        edges: [
          {
            account: CREATOR,
            creations: 1,
            funded_stroops: '4455340000',
            first_ledger: 43_611_183,
            last_ledger: 43_611_183,
            first_at: '2022-11-17T16:12:20Z',
            last_at: '2022-11-17T16:12:20Z',
          },
        ],
        total: 1,
        truncated: false,
      },
      sponsored_by: { edges: [], total: 0, truncated: false },
    },
    outbound: {
      created: {
        accounts: 12_345,
        creations: 20_000,
        funded_stroops: '123450000000',
        first_ledger: 100,
        last_ledger: 200,
        first_at: '2021-05-23T17:01:53Z',
        last_at: '2026-09-14T17:40:53Z',
      },
      // Non-zero, so the cross-link to the sponsor view is offered.
      sponsored: {
        accounts: 785_615,
        sponsorships_started: 2_585_729,
        revocations_issued: 1712,
        first_ledger: 300,
        last_ledger: 400,
        first_at: '2021-05-23T17:01:53Z',
        last_at: '2026-09-14T17:40:53Z',
      },
    },
    coverage: { creation: coverage, sponsorship: coverage },
    note: 'History, not live state.',
  },
};

const graphPage = {
  data: {
    ...graph.data,
    relation: 'created',
    edges: [
      {
        account: CREATOR,
        creations: 2,
        funded_stroops: '10000000',
        first_ledger: 100,
        last_ledger: 200,
        first_at: '2024-01-01T00:00:00Z',
        last_at: '2024-02-01T00:00:00Z',
      },
    ],
  },
};

const creators = {
  data: {
    creators: [
      {
        rank: 7,
        account: ACCOUNT,
        accounts_created: 12_345,
        funded_stroops: '123450000000',
        live_accounts: 9_876,
        // Deliberately not a stroop figure that renders as the same
        // string as live_accounts — 9,876 XLM beside 9,876 accounts
        // would make the assertions below pass on the wrong cell.
        live_stroops: '55500000000',
      },
    ],
    totals: { creators: 955_023 },
    computed_at: '2026-09-14T19:31:29Z',
  },
};

const state = {
  data: {
    account_id: ACCOUNT,
    exists: true,
    balance: '1234500000',
    trustlines: [{ asset: 'USDC-GA5Z', balance: '500000000' }],
  },
};

function route(path: string) {
  if (path.endsWith('/graph/history')) {
    // Exercised on its own in AccountRelationHistory.test.tsx; here the
    // point is that a 404 on this one arm does not take the page down.
    throw new Error(`404 Not Found on ${path}`);
  }
  if (path.endsWith('/graph')) return graphPage;
  if (path === '/v1/accounts/creators') return creators;
  if (path === `/v1/accounts/${ACCOUNT}`) return state;
  if (path === '/v1/price/batch') return { data: [] };
  throw new Error(`404 Not Found on ${path}`);
}

function renderView(
  account = ACCOUNT,
  relation: 'created' | 'sponsored' = 'created',
) {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(
    <QueryClientProvider client={qc}>
      <AccountRelationView account={account} relation={relation} />
    </QueryClientProvider>,
  );
}

describe('AccountRelationView', () => {
  beforeEach(() => {
    apiGet.mockReset();
    apiGet.mockImplementation(async (path: string, params?: unknown) => {
      // The summary call is the endpoint's BOUNDED default: no relation,
      // so it never asks for the unbounded outbound edge list.
      if (path.endsWith('/graph') && params === undefined) return graph;
      return route(path);
    });
  });

  /**
   * THE FINDING THIS GUARDS. The static export builds ONE `shell`
   * document per board and the CF Function serves it for every address,
   * so the component is rendered with `shell` at build time and with ''
   * on the server render before hydration. Firing the account fetches
   * for either would be a guaranteed 404 per page view, and rendering a
   * loading state forever would be worse.
   */
  it.each(['shell', '', 'not-an-account'])(
    'refuses %o as an address without calling the API',
    async (bad) => {
      renderView(bad);
      expect(screen.getByText('Not a Stellar account')).toBeInTheDocument();
      expect(apiGet).not.toHaveBeenCalled();
    },
  );

  it('says how an account id is shaped and links back to the board', () => {
    renderView('not-an-account');
    expect(screen.getByText(/56 characters/)).toBeInTheDocument();
    expect(
      screen.getByRole('link', { name: /Back to the account creators board/i }),
    ).toHaveAttribute('href', '/insights/creators/');
  });

  it('leads with the relation the page is about', async () => {
    renderView();
    // Waited for on a LOADED figure, not on the panel title: the loading
    // state carries the same title, so a title match would resolve before
    // any data had arrived.
    expect(
      await screen.findByText('operations, not addresses'),
    ).toBeInTheDocument();
    expect(screen.getByText('20,000')).toBeInTheDocument();
    expect(screen.getAllByText('12,345').length).toBeGreaterThan(0);
    expect(screen.getAllByText('Accounts created').length).toBeGreaterThan(0);
  });

  it('keeps the address whole and links to its full account page', async () => {
    renderView();
    expect(
      await screen.findByRole('heading', { level: 1, name: ACCOUNT }),
    ).toBeInTheDocument();
    expect(
      screen.getByRole('link', { name: /Full account page/ }),
    ).toHaveAttribute('href', `/accounts/${ACCOUNT}/`);
  });

  /**
   * An account can rank on both boards, and the two are different kinds
   * of fact that must not be added together. The cross-link is how a
   * reader gets the other answer without the page implying it is the
   * same one — and it is offered only when there is something there.
   */
  it('offers the sibling relation when the address is active in it', async () => {
    renderView();
    const link = await screen.findByRole('link', {
      name: /Also a sponsor of 785,615 accounts/,
    });
    expect(link).toHaveAttribute('href', `/insights/sponsors/${ACCOUNT}/`);
  });

  it('does not offer the sibling relation when there is nothing there', async () => {
    apiGet.mockImplementation(async (path: string, params?: unknown) => {
      if (path.endsWith('/graph') && params === undefined) {
        return {
          data: {
            ...graph.data,
            outbound: {
              ...graph.data.outbound,
              sponsored: {
                accounts: 0,
                sponsorships_started: 0,
                revocations_issued: 0,
              },
            },
          },
        };
      }
      return route(path);
    });
    renderView();
    await screen.findByText('This address as a creator');
    expect(screen.queryByText(/Also a sponsor/)).not.toBeInTheDocument();
  });

  it('shows who created this address, not only whom it created', async () => {
    renderView();
    expect(await screen.findByText('Created by')).toBeInTheDocument();
    expect(screen.getByTitle(CREATOR)).toBeInTheDocument();
  });

  it('shows the board standing and what the created set still holds', async () => {
    renderView();
    expect(await screen.findByText('#7')).toBeInTheDocument();
    expect(screen.getByText(/of 955,023 creators/)).toBeInTheDocument();
    expect(screen.getByText('Created set still live')).toBeInTheDocument();
    expect(screen.getByText('9,876')).toBeInTheDocument();
    expect(screen.getByText('XLM the set holds now')).toBeInTheDocument();
    expect(screen.getByText(/80\.0% of/)).toBeInTheDocument();
  });

  it('shows the assets the address itself holds', async () => {
    renderView();
    expect(await screen.findByText('Positions')).toBeInTheDocument();
  });

  it('lists the accounts it created, paginated', async () => {
    renderView();
    // The edges panel's own column control — "Accounts created" is also
    // a summary-stat label, so the heading text alone would not prove
    // the table rendered.
    expect(
      await screen.findByRole('button', { name: /Creations/ }),
    ).toBeInTheDocument();
    expect(
      screen.getByText(/785,615 in all|12,345 in all/),
    ).toBeInTheDocument();
  });

  /**
   * One arm being absent must not take the page with it: graph/history
   * 404s on a deployment whose API predates it, and every other panel
   * here reads a different endpoint.
   */
  it('survives the history arm being absent', async () => {
    renderView();
    expect(
      await screen.findByText('This address as a creator'),
    ).toBeInTheDocument();
    expect(await screen.findByText(/does not serve/i)).toBeInTheDocument();
    expect(screen.getByText('Positions')).toBeInTheDocument();
  });

  it('calls a warming graph warming rather than broken', async () => {
    apiGet.mockRejectedValue(
      new Error(`503 Service Unavailable on /v1/accounts/${ACCOUNT}/graph`),
    );
    renderView();
    expect(
      await screen.findByText(/sponsorship\/creation graph is warming/i),
    ).toBeInTheDocument();
  });
});
