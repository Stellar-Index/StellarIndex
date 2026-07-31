import { describe, it, expect, vi } from 'vitest';
import { render, screen, waitFor } from '@testing-library/react';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';

import { HoldersTabPanel } from './HoldersTabPanel';

// Native XLM has no trustlines — its holders board is the account-balance
// ranking (backend fix 2026-07-31). The panel's empty state must therefore
// never blame "trustline … backfill" for XLM: that copy was simply false
// for native (the API returned holder_count 0 by construction pre-fix, and
// post-fix an empty board means the ranking is warming/unavailable). Issued
// assets keep the backfill copy — for them it is accurate.
vi.mock('@/api/client', async () => {
  const actual = await vi.importActual<typeof import('@/api/client')>('@/api/client');
  return { ...actual, apiGet: vi.fn() };
});

import { apiGet } from '@/api/client';

function renderPanel(assetID: string) {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(
    <QueryClientProvider client={client}>
      <HoldersTabPanel assetID={assetID} />
    </QueryClientProvider>,
  );
}

describe('HoldersTabPanel', () => {
  it('renders the native holders board (account balances) when the API serves data', async () => {
    vi.mocked(apiGet).mockResolvedValue({
      data: {
        asset: 'native',
        holder_count: 9915982,
        holders: [
          {
            account_id: 'GALAXYVOIDAOPZTDLHILAJQKCVVFMD4IKLXLSZV5YHO7VY74IWZILUTO',
            balance: '554421152474348098',
          },
        ],
      },
    });
    renderPanel('native');
    await waitFor(() => expect(screen.getByText(/^GALAXYVO/)).toBeInTheDocument());
    // 9,915,982 holders → shared formatCompact (maximumFractionDigits: 2)
    // renders "9.92M" in the panel title.
    expect(screen.getByText(/Holders \(9\.92M\)/)).toBeInTheDocument();
  });

  it('does NOT blame the trustline backfill for an empty native board', async () => {
    vi.mocked(apiGet).mockResolvedValue({ data: { asset: 'native', holder_count: 0, holders: [] } });
    renderPanel('native');
    await waitFor(() =>
      expect(screen.getByText(/XLM holder data is not available right now/)).toBeInTheDocument(),
    );
    expect(screen.queryByText(/entry-change backfill/)).not.toBeInTheDocument();
  });

  it('keeps the (accurate) backfill copy for an empty issued-asset board', async () => {
    vi.mocked(apiGet).mockResolvedValue({
      data: { asset: 'FOO-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN', holder_count: 0, holders: [] },
    });
    renderPanel('FOO-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN');
    await waitFor(() =>
      expect(screen.getByText(/entry-change backfill progresses/)).toBeInTheDocument(),
    );
  });
});
