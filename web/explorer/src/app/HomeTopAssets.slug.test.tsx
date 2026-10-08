import { describe, it, expect, vi, beforeEach } from 'vitest';
import { render, screen, waitFor } from '@testing-library/react';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';

vi.mock('@/api/client', async () => {
  const actual =
    await vi.importActual<typeof import('@/api/client')>('@/api/client');
  return { ...actual, apiGet: vi.fn() };
});

import { apiGet } from '@/api/client';
import { HomeTopAssets } from './HomeTopAssets';

// Every sibling identity dereference in this file was
// migrated to coinSlug(coin) (falls back slug -> asset_id) except the
// row's sub-label, which still read the bare, optional `slug` field —
// a classic asset with no catalogue slug rendered a blank sub-label
// instead of falling back to its asset_id.
const ISSUER = 'GBLTXF46JTCGMWFJASQLVXMMA36IPYTDCN4EN73HRXCGDCGYBZM3A6VD';
const NO_SLUG_ROW = {
  kind: 'stellar_asset',
  asset_id: `NOSLUG-${ISSUER}`,
  type: 'classic',
  code: 'NOSLUG',
  // no `slug` — the field is omitted on the wire for a classic asset with
  // no catalogue identity.
  decimals: 7,
  sep1_status: 'not_applicable',
  volume_24h_usd: '100.00',
  observation_count: 10,
};

function renderWithQuery(ui: React.ReactElement) {
  const qc = new QueryClient({
    defaultOptions: { queries: { retry: false, gcTime: 0 } },
  });
  return render(<QueryClientProvider client={qc}>{ui}</QueryClientProvider>);
}

describe('HomeTopAssets row sub-label falls back like every other identity read', () => {
  beforeEach(() => {
    vi.mocked(apiGet).mockReset();
    vi.mocked(apiGet).mockImplementation(async (path: string) => {
      if (path.startsWith('/v1/assets/verified')) return { data: [] };
      if (path.startsWith('/v1/assets/native')) return { data: null };
      if (path.startsWith('/v1/assets')) return { data: [NO_SLUG_ROW] };
      return { data: [] };
    });
  });

  it('renders the asset_id, not a blank sub-label, when slug is absent', async () => {
    renderWithQuery(<HomeTopAssets />);
    await waitFor(() => {
      expect(screen.getByText(NO_SLUG_ROW.asset_id)).toBeInTheDocument();
    });
  });
});
