import { describe, it, expect, vi, beforeEach } from 'vitest';
import { render, screen, waitFor } from '@testing-library/react';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';

vi.mock('@/api/client', async () => {
  const actual = await vi.importActual<typeof import('@/api/client')>('@/api/client');
  return { ...actual, apiGet: vi.fn() };
});

import { apiGet } from '@/api/client';
import { HomeTopAssets } from './HomeTopAssets';

// Regression test for the 2026-08-31 explorer outage.
//
// /v1/assets began honouring `order_by=volume_24h_usd_desc` — the
// ranking this page's own caption had always claimed — instead of
// silently serving the top ten by ALL-TIME observation count. That
// changed WHICH assets appear: observation-count order is classic-only
// in practice, so every row happened to carry a `code`. Volume order
// surfaces Soroban contract assets at once (four in the live top ten),
// and a Soroban asset has NO code — the API omits the field entirely
// (`json:"code,omitempty"`).
//
// `iconForCode(code)` then called `.toUpperCase()` on undefined. The
// home page renders inside the root layout, so the throw escaped to
// global-error and took down the ENTIRE application shell — every
// route, not just this table.
//
// It type-checked cleanly throughout: the OpenAPI spec marked `code`
// required, so the generated type declared `code: string`. Spec and
// generated types agreed with each other and both disagreed with the
// server. The spec is corrected; this pins the render so the row can
// never regress to dereferencing a code that isn't there.

// The exact shape production returns for a Soroban row: no `code` key.
const SOROBAN_ROW = {
  kind: 'stellar_asset',
  asset_id: 'CAS3J7GYLGXMF6TDJBBYYSE3HQ6BBSMLNUQ34T6TZMYMW2EVH34XOWMA',
  type: 'soroban',
  slug: 'CAS3J7GYLGXMF6TDJBBYYSE3HQ6BBSMLNUQ34T6TZMYMW2EVH34XOWMA',
  decimals: 7,
  sep1_status: 'none',
  volume_24h_usd: '18758.42',
  observation_count: 1837,
};

const CLASSIC_ROW = {
  kind: 'stellar_asset',
  asset_id: 'USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN',
  type: 'classic',
  code: 'USDC',
  slug: 'USDC',
  decimals: 7,
  sep1_status: 'ok',
  volume_24h_usd: '734332.72',
  observation_count: 41698077,
};

function renderWithQuery(ui: React.ReactElement) {
  const qc = new QueryClient({
    defaultOptions: { queries: { retry: false, gcTime: 0 } },
  });
  return render(<QueryClientProvider client={qc}>{ui}</QueryClientProvider>);
}

describe('HomeTopAssets with a code-less Soroban asset', () => {
  beforeEach(() => {
    vi.mocked(apiGet).mockReset();
    vi.mocked(apiGet).mockImplementation(async (path: string) => {
      if (path.startsWith('/v1/assets/verified')) return { data: [] };
      // Order matters: /v1/assets/native returns a SINGLE row, not a
      // list. Matching it with the generic /v1/assets branch handed the
      // component an array where it expected an object, which crashed on
      // a different field entirely and masked what this test is for.
      if (path.startsWith('/v1/assets/native')) return { data: null };
      if (path.startsWith('/v1/assets')) {
        return { data: [CLASSIC_ROW, SOROBAN_ROW] };
      }
      return { data: [] };
    });
  });

  it('renders a row whose asset has no code, without crashing', async () => {
    renderWithQuery(<HomeTopAssets />);

    // The classic row proves the table actually rendered rows at all —
    // without this the test would pass on an empty table and assert
    // nothing, which is how the original gap survived 548 green tests.
    // getAllBy: the code appears both as the row label and inside the
    // icon fallback.
    await waitFor(() => {
      expect(screen.getAllByText('USDC').length).toBeGreaterThan(0);
    });

    // The code-less row must render an identity rather than a blank
    // cell: a truncated contract id.
    expect(screen.getAllByText(/CAS3…OWMA/).length).toBeGreaterThan(0);
  });
});
