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

  it('asks the board for one account rather than paging it', async () => {
    renderPanel(OTHER);
    await screen.findByText('#1');
    expect(apiGet).toHaveBeenCalledWith('/v1/accounts/sponsors', {
      account: OTHER,
    });
  });

  it('places a ranked address against the whole population', async () => {
    renderPanel(OTHER);
    expect(await screen.findByText('#1')).toBeInTheDocument();
    expect(screen.getByText('of 2,427 sponsors')).toBeInTheDocument();
  });

  it('reads an empty keyed result as "no row", not as a missed page', async () => {
    apiGet.mockResolvedValue({
      data: {
        sponsors: [],
        totals: { sponsors: 2_427 },
        computed_at: sponsors.data.computed_at,
      },
    });
    renderPanel();
    const note = await screen.findByText(/holds no row on the/i);
    expect(note.textContent).toContain('2,427');
    expect(screen.queryByText('Rank')).not.toBeInTheDocument();
  });

  /**
   * THE FINDING THIS GUARDS. `?account=` is a keyed read, but a
   * deployment whose API predates it IGNORES the parameter and serves
   * the default page. Taking rows[0] on that response would publish the
   * top-ranked account's rank as this address's — a wrong number
   * wearing the right label, which is worse than an absent one. The
   * panel matches the address instead of trusting the filter.
   */
  it("never claims another account's rank when the filter was ignored", async () => {
    renderPanel(); // ACCOUNT, while the stub answers with OTHER at rank 1
    const note = await screen.findByText(/holds no row on the/i);
    expect(note).toBeInTheDocument();
    expect(screen.queryByText('#1')).not.toBeInTheDocument();
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

  // The generated types call both fields required, but the panel reads a
  // wire it does not control: a skewed or partial deployment can serve a
  // body with no board in it. That is a board not yet built — not an
  // address with no row on it, and not a reason to blank the page.
  it('calls a body that carries no board warming instead of crashing', async () => {
    apiGet.mockResolvedValue({ data: {} });
    renderPanel();
    expect(await screen.findByText(/warming/i)).toBeInTheDocument();
    expect(screen.queryByText(/holds no row on the/i)).not.toBeInTheDocument();
  });

  it('still ranks the address when the body carries no totals', async () => {
    apiGet.mockResolvedValue({
      data: {
        sponsors: sponsors.data.sponsors,
        computed_at: sponsors.data.computed_at,
      },
    });
    renderPanel(OTHER);
    expect(await screen.findByText('#1')).toBeInTheDocument();
    expect(screen.queryByText(/of .* sponsors/)).not.toBeInTheDocument();
  });
});
