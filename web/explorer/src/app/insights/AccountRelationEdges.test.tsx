import { describe, it, expect, beforeEach, vi } from 'vitest';
import { fireEvent, render, screen, within } from '@testing-library/react';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';

import { AccountRelationEdges } from './AccountRelationEdges';

const ACCOUNT = 'GDB3RSSWTUXO7MBTNMHUP3DRBIUR3QRV2CVFRAKMN4GM2B4QNGEUT6CU';

const apiGet = vi.hoisted(() => vi.fn());
vi.mock('@/api/client', async (importOriginal) => {
  const mod = await importOriginal<typeof import('@/api/client')>();
  return { ...mod, apiGet };
});

function edge(
  suffix: string,
  over: {
    creations?: number;
    sponsorships_started?: number;
    funded_stroops?: string;
  },
) {
  return {
    account: `G${suffix.padEnd(55, 'A')}`,
    first_ledger: 100,
    last_ledger: 200,
    first_at: '2024-01-01T00:00:00Z',
    last_at: '2024-06-01T00:00:00Z',
    ...over,
  };
}

// Served ascending by account id — that ordering IS the cursor — so the
// biggest funder is deliberately NOT first in the payload.
const BIG = '9007199254740993';
const SMALL = '9007199254740992';

const graph = {
  data: {
    account: ACCOUNT,
    inbound: {
      created_by: { edges: [], total: 0, truncated: false },
      sponsored_by: { edges: [], total: 0, truncated: false },
    },
    outbound: {
      created: { accounts: 3, creations: 5, funded_stroops: '30' },
      sponsored: {
        accounts: 3,
        sponsorships_started: 5,
        revocations_issued: 0,
      },
    },
    relation: 'created',
    edges: [
      edge('AA', { creations: 1, funded_stroops: SMALL }),
      edge('BB', { creations: 9, funded_stroops: '10' }),
      edge('CC', { creations: 4, funded_stroops: BIG }),
    ],
    next_cursor: `G${'CC'.padEnd(55, 'A')}`,
    coverage: {
      creation: {
        from_ledger: 3,
        thru_ledger: 64_428_050,
        from_time: '2015-09-30T17:15:54Z',
        thru_time: '2026-09-14T17:45:39Z',
        computed_at: '2026-09-14T17:57:59Z',
      },
      sponsorship: {
        from_ledger: 32_747_295,
        thru_ledger: 64_428_050,
        from_time: '2020-11-23T16:00:18Z',
        thru_time: '2026-09-14T17:45:39Z',
        computed_at: '2026-09-14T17:57:59Z',
      },
    },
    note: 'History, not live state.',
  },
};

function renderPanel(relation: 'created' | 'sponsored' = 'created') {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(
    <QueryClientProvider client={qc}>
      <AccountRelationEdges
        account={ACCOUNT}
        relation={relation}
        total={785_615}
      />
    </QueryClientProvider>,
  );
}

function rowOrder() {
  const body = screen.getAllByRole('rowgroup')[1];
  return within(body)
    .getAllByRole('row')
    .map((r) => r.textContent?.slice(0, 12) ?? '');
}

describe('AccountRelationEdges', () => {
  beforeEach(() => {
    apiGet.mockReset();
    apiGet.mockResolvedValue(graph);
  });

  it('lists one row per counterparty and links each to its account page', async () => {
    renderPanel();
    const link = await screen.findByTitle(`G${'AA'.padEnd(55, 'A')}`);
    expect(link).toHaveAttribute(
      'href',
      `/accounts/${encodeURIComponent(`G${'AA'.padEnd(55, 'A')}`)}/`,
    );
    expect(rowOrder()).toHaveLength(3);
  });

  it('reorders the page when a column header is used, and marks the sorted column', async () => {
    renderPanel();
    await screen.findByRole('button', { name: /Creations/ });

    const served = rowOrder();
    fireEvent.click(screen.getByRole('button', { name: /Creations/ }));
    const sorted = rowOrder();

    // A quantity opens largest-first: BB (9) leads, then CC (4), then AA (1).
    expect(sorted).not.toEqual(served);
    expect(sorted[0]).toContain('BB');
    expect(
      screen.getByRole('columnheader', { name: /Creations/ }),
    ).toHaveAttribute('aria-sort', 'descending');

    // Clicking again flips direction rather than re-sorting the same way.
    fireEvent.click(screen.getByRole('button', { name: /Creations/ }));
    expect(rowOrder()[0]).toContain('AA');
    expect(
      screen.getByRole('columnheader', { name: /Creations/ }),
    ).toHaveAttribute('aria-sort', 'ascending');
  });

  /**
   * THE FINDING THIS GUARDS. funded_stroops is an exact decimal string.
   * Sorting it through Number() rounds anything past 2^53 to the same
   * float, so the two biggest funders on the page — the rows the sort
   * exists to surface — would stay in whatever order they arrived.
   */
  it('orders the funded column exactly, past the float-precision ceiling', async () => {
    renderPanel();
    await screen.findByRole('button', { name: /XLM funded/ });
    expect(Number(BIG)).toBe(Number(SMALL));

    fireEvent.click(screen.getByRole('button', { name: /XLM funded/ }));
    expect(rowOrder()[0]).toContain('CC');
  });

  /**
   * The sort is page-local and must say so. The endpoint is keyset-paged
   * by counterparty account id, so there is no whole-set ranking to ask
   * for; presenting a 50-row sort as a leaderboard over 785,615 rows
   * would be the same error as reading a snapshot as a total.
   */
  it('states that the headers sort the page and not the whole set', async () => {
    renderPanel();
    const note = await screen.findByText(/sort/i, { selector: 'p' });
    expect(note.textContent).toMatch(/this page/i);
    expect(note.textContent).toContain('785,615');
  });

  it('pages forward on the served cursor and can return to the first page', async () => {
    renderPanel();
    const more = await screen.findByRole('button', { name: 'Next page' });

    fireEvent.click(more);

    expect(apiGet).toHaveBeenLastCalledWith(
      `/v1/accounts/${ACCOUNT}/graph`,
      expect.objectContaining({ cursor: `G${'CC'.padEnd(55, 'A')}` }),
    );
    expect(
      await screen.findByRole('button', { name: 'First page' }),
    ).toBeInTheDocument();
  });

  it('omits the funded column on the sponsorship arm, which moves no balance', async () => {
    apiGet.mockResolvedValue({
      data: {
        ...graph.data,
        relation: 'sponsored',
        edges: graph.data.edges.map((e) => ({
          account: e.account,
          sponsorships_started: e.creations,
          first_ledger: e.first_ledger,
          last_ledger: e.last_ledger,
          first_at: e.first_at,
          last_at: e.last_at,
        })),
      },
    });
    renderPanel('sponsored');

    expect(
      await screen.findByRole('button', { name: /Started/ }),
    ).toBeInTheDocument();
    expect(
      screen.queryByRole('button', { name: /XLM funded/ }),
    ).not.toBeInTheDocument();
    expect(
      screen.getByText(/never a count of sponsorships in force/i),
    ).toBeInTheDocument();
  });

  it('reads an empty edge list as an answer rather than a failure', async () => {
    apiGet.mockResolvedValue({ data: { ...graph.data, edges: [] } });
    renderPanel();
    expect(
      await screen.findByText(/no accounts created in the covered span/i),
    ).toBeInTheDocument();
  });

  it('calls a warming graph warming', async () => {
    apiGet.mockRejectedValue(
      new Error(`503 Service Unavailable on /v1/accounts/${ACCOUNT}/graph`),
    );
    renderPanel();
    expect(await screen.findByText(/warming/i)).toBeInTheDocument();
  });
});
