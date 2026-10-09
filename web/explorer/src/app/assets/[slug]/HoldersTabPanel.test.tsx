import { describe, it, expect, vi } from 'vitest';
import { render, screen, waitFor } from '@testing-library/react';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';

import { HoldersTabPanel, servedShares } from './HoldersTabPanel';

// Native XLM has no trustlines — its holders board is the account-balance
// ranking. The panel's empty state must therefore
// never blame "trustline … backfill" for XLM: that copy was simply false
// for native (an empty board means the ranking is warming/unavailable). Issued
// assets keep the backfill copy — for them it is accurate.
vi.mock('@/api/client', async () => {
  const actual =
    await vi.importActual<typeof import('@/api/client')>('@/api/client');
  return { ...actual, apiGet: vi.fn() };
});

import { apiGet } from '@/api/client';

function renderPanel(assetID: string) {
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  });
  return render(
    <QueryClientProvider client={client}>
      <HoldersTabPanel assetID={assetID} />
    </QueryClientProvider>,
  );
}

describe('HoldersTabPanel', () => {
  it('rounds an exact 12.5% top-10 share as 13%, not float 12%', async () => {
    vi.mocked(apiGet).mockResolvedValue({
      data: {
        asset: 'native',
        holder_count: 11,
        holders: [
          ...Array.from({ length: 10 }, (_, i) => ({
            account_id: `G${String(i).padStart(55, 'A')}`,
            balance: '15',
          })),
          { account_id: `G${'B'.repeat(55)}`, balance: '1050' },
        ],
      },
    });
    renderPanel('native');
    expect(await screen.findByText('13%')).toBeInTheDocument();
  });

  it('renders the native holders board (account balances) when the API serves data', async () => {
    vi.mocked(apiGet).mockResolvedValue({
      data: {
        asset: 'native',
        holder_count: 9915982,
        holders: [
          {
            account_id:
              'GALAXYVOIDAOPZTDLHILAJQKCVVFMD4IKLXLSZV5YHO7VY74IWZILUTO',
            balance: '554421152474348098',
          },
        ],
      },
    });
    renderPanel('native');
    await waitFor(() =>
      expect(screen.getByText(/^GALAXYVO/)).toBeInTheDocument(),
    );
    // 9,915,982 holders → shared formatCompact (maximumFractionDigits: 2)
    // renders "9.92M" in the panel title.
    expect(screen.getByText(/Holders \(9\.92M\)/)).toBeInTheDocument();
  });

  it('does NOT blame the trustline backfill for an empty native board', async () => {
    vi.mocked(apiGet).mockResolvedValue({
      data: { asset: 'native', holder_count: 0, holders: [] },
    });
    renderPanel('native');
    await waitFor(() =>
      expect(
        screen.getByText(/XLM holder data is not available right now/),
      ).toBeInTheDocument(),
    );
    expect(screen.queryByText(/entry-change backfill/)).not.toBeInTheDocument();
  });

  it('keeps the (accurate) backfill copy for an empty issued-asset board', async () => {
    vi.mocked(apiGet).mockResolvedValue({
      data: {
        asset: 'FOO-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN',
        holder_count: 0,
        holders: [],
      },
    });
    renderPanel('FOO-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN');
    await waitFor(() =>
      expect(
        screen.getByText(/entry-change backfill progresses/),
      ).toBeInTheDocument(),
    );
  });
});

describe('servedShares', () => {
  it('splits exactly over the served rows and clamps negatives', () => {
    expect(
      servedShares([{ balance: '30' }, { balance: '10' }, { balance: '-5' }]),
    ).toEqual([75, 25, 0]);
  });

  it('keeps precision for balances above 2^53', () => {
    expect(
      servedShares([
        { balance: '9007199254740993' },
        { balance: '9007199254740993' },
      ]),
    ).toEqual([50, 50]);
  });

  it('returns null for a missing balance', () => {
    expect(servedShares([{ balance: '10' }, {}])[1]).toBeNull();
  });
});
