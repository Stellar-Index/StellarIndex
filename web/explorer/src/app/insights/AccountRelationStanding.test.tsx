import { describe, it, expect, beforeEach, vi } from 'vitest';
import { render, screen } from '@testing-library/react';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';

import { AccountRelationStanding } from './AccountRelationStanding';

const ACCOUNT = 'GDB3RSSWTUXO7MBTNMHUP3DRBIUR3QRV2CVFRAKMN4GM2B4QNGEUT6CU';
const OTHER = 'GAUA7XL5K54CC2DDGP77FJ2YBHRJLT36CPZDXWPM6MP7MANOGG77PNJU';

const apiGet = vi.hoisted(() => vi.fn());
vi.mock('@/api/client', async (importOriginal) => {
  const mod = await importOriginal<typeof import('@/api/client')>();
  return { ...mod, apiGet };
});

const sponsors = {
  data: {
    sponsors: [
      {
        rank: 1,
        account: OTHER,
        sponsorships_started: 1_432_881,
        distinct_sponsored: 1_296_181,
        revocations_issued: 41,
      },
    ],
    totals: { sponsors: 2_427 },
    computed_at: '2026-09-14T17:57:59Z',
  },
};

function renderPanel(account = ACCOUNT) {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(
    <QueryClientProvider client={qc}>
      <AccountRelationStanding account={account} relation="sponsored" />
    </QueryClientProvider>,
  );
}

describe('AccountRelationStanding', () => {
  beforeEach(() => {
    apiGet.mockReset();
    apiGet.mockResolvedValue(sponsors);
  });

  it('asks for the deepest page the board endpoint serves', async () => {
    renderPanel(OTHER);
    await screen.findByText('#1');
    expect(apiGet).toHaveBeenCalledWith('/v1/accounts/sponsors', {
      limit: 500,
    });
  });

  it('places a ranked address against the whole population', async () => {
    renderPanel(OTHER);
    expect(await screen.findByText('#1')).toBeInTheDocument();
    expect(screen.getByText('of 2,427 sponsors')).toBeInTheDocument();
  });

  /**
   * THE FINDING THIS GUARDS. The board endpoint takes no account filter
   * and caps a page at 500 rows, so an address outside that page has an
   * UNKNOWN rank, not a low one. Rendering the absence as "unranked" —
   * or as a blank cell — would turn a reach limit of the lookup into a
   * claim about the address, and the address's own figures on this page
   * are exact whether or not the board reaches it.
   */
  it('says an unfound rank is beyond the lookup, not a low rank', async () => {
    renderPanel();
    const note = await screen.findByText(/not in the top 500/i);
    expect(note.textContent).toMatch(/unknown here rather than low/i);
    expect(note.textContent).toContain('2,427');
    expect(screen.queryByText('Rank')).not.toBeInTheDocument();
  });

  it('links back to the board it could not find the address on', async () => {
    renderPanel();
    expect(
      await screen.findByRole('link', { name: /account sponsors board/i }),
    ).toHaveAttribute('href', '/insights/sponsors/');
  });

  it('dates the snapshot rather than presenting it as live state', async () => {
    renderPanel(OTHER);
    expect(
      await screen.findByText(/Rollup snapshot computed/),
    ).toBeInTheDocument();
  });

  it('calls a warming board warming', async () => {
    apiGet.mockRejectedValue(new Error('503 Service Unavailable'));
    renderPanel();
    expect(await screen.findByText(/warming/i)).toBeInTheDocument();
  });
});
