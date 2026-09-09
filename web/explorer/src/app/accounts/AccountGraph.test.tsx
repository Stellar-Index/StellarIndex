import { describe, it, expect, vi } from 'vitest';
import { fireEvent, render, screen, waitFor } from '@testing-library/react';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';

vi.mock('@/api/client', async () => {
  const actual =
    await vi.importActual<typeof import('@/api/client')>('@/api/client');
  return { ...actual, apiGet: vi.fn() };
});

import { apiGet } from '@/api/client';
import { AccountGraphPanel } from './AccountGraph';

const G = 'GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN';
const CREATOR = 'GAUA7XL5K54CC2DDGP77FJ2YBHRJLT36CPZDXWPM6MP7MANOGG77PNJU';
const SPONSOR = 'GDB3RSSWTUXO7MBTNMHUP3DRBIUR3QRV2CVFRAKMN4GM2B4QNGEUT6CU';

const NOTE =
  'History, not live state. Creation edges are immutable. Sponsorship edges count ' +
  'arrangements STARTED and are never a count of sponsorships in force.';

const COVERAGE = {
  creation: {
    from_ledger: 3,
    thru_ledger: 64346048,
    from_time: '2015-09-30T16:46:00Z',
    thru_time: '2026-09-09T11:02:56Z',
    computed_at: '2026-09-09T12:00:00Z',
  },
  sponsorship: {
    from_ledger: 32747295,
    thru_ledger: 64346120,
    from_time: '2020-11-23T15:20:18Z',
    thru_time: '2026-09-09T11:14:08Z',
    computed_at: '2026-09-09T12:00:00Z',
  },
};

function renderWithClient(ui: React.ReactElement) {
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  });
  return render(
    <QueryClientProvider client={client}>{ui}</QueryClientProvider>,
  );
}

function edge(i: number) {
  return {
    // Distinct, ordered, strkey-shaped fixture ids. The panel only ever
    // renders and links them; the API validates the real thing.
    account: `GAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA${String(i).padStart(5, '0')}`,
    sponsorships_started: 1,
    first_ledger: 60000000 + i,
    last_ledger: 60000000 + i,
    first_at: '2026-01-01T00:00:00Z',
    last_at: '2026-01-01T00:00:00Z',
  };
}

describe('AccountGraphPanel', () => {
  it('renders both inbound directions and never asks for an outbound edge list by default', async () => {
    vi.mocked(apiGet).mockResolvedValue({
      data: {
        account: G,
        inbound: {
          created_by: {
            edges: [
              {
                account: CREATOR,
                creations: 3,
                funded_stroops: '30000000',
                first_ledger: 52651627,
                last_ledger: 59446198,
                first_at: '2024-07-20T03:10:32Z',
                last_at: '2025-10-19T01:44:46Z',
              },
            ],
            total: 1,
            truncated: false,
          },
          sponsored_by: {
            edges: [
              {
                account: SPONSOR,
                sponsorships_started: 2,
                first_ledger: 60000001,
                last_ledger: 63000004,
                first_at: '2025-10-19T01:44:46Z',
                last_at: '2026-08-17T19:43:29Z',
              },
            ],
            total: 1,
            truncated: false,
          },
        },
        outbound: {
          created: { accounts: 0, creations: 0, funded_stroops: '0' },
          sponsored: {
            accounts: 0,
            sponsorships_started: 0,
            revocations_issued: 0,
          },
        },
        coverage: COVERAGE,
        note: NOTE,
      },
    });

    renderWithClient(<AccountGraphPanel id={G} />);

    // Both directions of the graph are on the page.
    await waitFor(() =>
      expect(screen.getByText('Created by')).toBeInTheDocument(),
    );
    expect(screen.getByText('Sponsored by')).toBeInTheDocument();
    // A recycled address's repeat creations are stated, not collapsed.
    expect(screen.getByText('3× created')).toBeInTheDocument();
    expect(screen.getByText('2 started')).toBeInTheDocument();

    // The default request carries NO relation: the unbounded direction
    // must be opt-in, or every account page would pull an edge list that
    // for the busiest sponsor is 785,543 rows.
    const params = vi.mocked(apiGet).mock.calls[0]?.[1] ?? {};
    expect(params).not.toHaveProperty('relation');
  });

  it('bounds the outbound list and states how far into the set the page reaches', async () => {
    const first = Array.from({ length: 25 }, (_, i) => edge(i));
    vi.mocked(apiGet).mockImplementation(async (_path, params) => ({
      data: {
        account: G,
        inbound: {
          created_by: { edges: [], total: 0, truncated: false },
          sponsored_by: { edges: [], total: 0, truncated: false },
        },
        outbound: {
          created: { accounts: 0, creations: 0, funded_stroops: '0' },
          sponsored: {
            accounts: 785543,
            sponsorships_started: 2581112,
            revocations_issued: 1683,
          },
        },
        ...(params?.relation
          ? {
              relation: 'sponsored',
              edges: first,
              next_cursor: first[24].account,
            }
          : {}),
        coverage: COVERAGE,
        note: NOTE,
      },
    }));

    renderWithClient(<AccountGraphPanel id={G} />);
    await waitFor(() =>
      expect(screen.getByText('785,543')).toBeInTheDocument(),
    );

    fireEvent.click(screen.getByRole('button', { name: 'Accounts sponsored' }));

    // A page, not the set — and the panel says so rather than letting 25
    // rows read as the whole relationship.
    await waitFor(() =>
      expect(screen.getByText('Showing 25 of 785,543.')).toBeInTheDocument(),
    );
    const call = vi.mocked(apiGet).mock.calls.at(-1);
    expect(call?.[1]).toMatchObject({ relation: 'sponsored', limit: 25 });
    expect(screen.getByRole('button', { name: /More/ })).toBeInTheDocument();
  });

  it('renders revocations as an account-level count and never as edge state', async () => {
    vi.mocked(apiGet).mockResolvedValue({
      data: {
        account: G,
        inbound: {
          created_by: { edges: [], total: 0, truncated: false },
          sponsored_by: { edges: [], total: 0, truncated: false },
        },
        outbound: {
          created: { accounts: 0, creations: 0, funded_stroops: '0' },
          sponsored: {
            accounts: 4,
            sponsorships_started: 9,
            revocations_issued: 3,
          },
        },
        coverage: COVERAGE,
        note: NOTE,
      },
    });

    renderWithClient(<AccountGraphPanel id={G} />);

    await waitFor(() =>
      expect(screen.getByText('Revocations issued')).toBeInTheDocument(),
    );
    expect(
      screen.getByText('Revocations issued').nextElementSibling?.textContent,
    ).toBe('3');
    // The history-not-live-state contract is rendered with the numbers.
    expect(screen.getByText(/History, not live state/)).toBeInTheDocument();
    expect(screen.queryByText(/currently sponsoring/i)).not.toBeInTheDocument();
  });

  it('renders an account with no creator and no sponsor as that, not as an error', async () => {
    vi.mocked(apiGet).mockResolvedValue({
      data: {
        account: G,
        inbound: {
          created_by: { edges: [], total: 0, truncated: false },
          sponsored_by: { edges: [], total: 0, truncated: false },
        },
        outbound: {
          created: { accounts: 0, creations: 0, funded_stroops: '0' },
          sponsored: {
            accounts: 0,
            sponsorships_started: 0,
            revocations_issued: 0,
          },
        },
        coverage: COVERAGE,
        note: NOTE,
      },
    });

    renderWithClient(<AccountGraphPanel id={G} />);

    await waitFor(() =>
      expect(
        screen.getByText(/No CreateAccount operation for this address/),
      ).toBeInTheDocument(),
    );
    expect(
      screen.getByText(/No account has begun a sponsorship arrangement/),
    ).toBeInTheDocument();
    // No direction selector: there is nothing to page into, so the
    // unbounded side is not even offered.
    expect(
      screen.queryByRole('group', { name: 'Outbound graph direction' }),
    ).not.toBeInTheDocument();
    // The coverage that qualifies the emptiness is still shown, and the
    // sponsorship floor is presented as the feature's genesis.
    expect(screen.getByText(/32,747,295/)).toBeInTheDocument();
  });

  it('says when the inbound cap bites instead of letting a short list read as complete', async () => {
    vi.mocked(apiGet).mockResolvedValue({
      data: {
        account: G,
        inbound: {
          created_by: { edges: [], total: 0, truncated: false },
          sponsored_by: {
            edges: [
              {
                account: SPONSOR,
                sponsorships_started: 1,
                first_ledger: 60000001,
                last_ledger: 60000001,
                first_at: '2025-10-19T01:44:46Z',
                last_at: '2025-10-19T01:44:46Z',
              },
            ],
            total: 40,
            truncated: true,
          },
        },
        outbound: {
          created: { accounts: 0, creations: 0, funded_stroops: '0' },
          sponsored: {
            accounts: 0,
            sponsorships_started: 0,
            revocations_issued: 0,
          },
        },
        coverage: COVERAGE,
        note: NOTE,
      },
    });

    renderWithClient(<AccountGraphPanel id={G} />);

    await waitFor(() =>
      expect(screen.getByText('Showing 1 of 40.')).toBeInTheDocument(),
    );
  });

  it('falls back to a warming message rather than an empty graph', async () => {
    vi.mocked(apiGet).mockRejectedValueOnce(new Error('503'));
    renderWithClient(<AccountGraphPanel id={G} />);
    expect(await screen.findByText(/warming/i)).toBeInTheDocument();
  });
});
