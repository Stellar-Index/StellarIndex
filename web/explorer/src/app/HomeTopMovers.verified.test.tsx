import { describe, it, expect, vi } from 'vitest';
import { render, screen, waitFor } from '@testing-library/react';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';

vi.mock('@/api/client', async () => {
  const actual =
    await vi.importActual<typeof import('@/api/client')>('@/api/client');
  return { ...actual, apiGet: vi.fn() };
});

import { apiGet } from '@/api/client';
import { HomeTopMovers } from './HomeTopMovers';

// The verified badge follows the catalogue's asset_id. The listing's slug
// falls back to the bare code, so a look-alike issuer of a verified code
// must not be badged.
const REAL = `USDC-G${'A'.repeat(55)}`;
const FAKE = `USDC-G${'B'.repeat(55)}`;

const row = (asset_id: string) => ({
  kind: 'stellar_asset',
  asset_id,
  type: 'classic',
  code: 'USDC',
  slug: 'usdc',
  decimals: 7,
  sep1_status: 'not_applicable',
  volume_24h_usd: '100.00',
  change_24h_pct: '5.5',
  observation_count: 10,
});

function renderWithQuery(ui: React.ReactElement) {
  const qc = new QueryClient({
    defaultOptions: { queries: { retry: false, gcTime: 0 } },
  });
  return render(<QueryClientProvider client={qc}>{ui}</QueryClientProvider>);
}

function mockApi(rows: ReturnType<typeof row>[]) {
  vi.mocked(apiGet).mockReset();
  vi.mocked(apiGet).mockImplementation(async (path: string) => {
    if (path.startsWith('/v1/assets/verified'))
      return { data: [{ slug: 'usdc', asset_id: REAL }] };
    if (path.startsWith('/v1/assets/native')) return { data: null };
    if (path.startsWith('/v1/assets')) return { data: rows };
    return { data: [] };
  });
}

describe('HomeTopMovers verified badge', () => {
  it('badges the catalogued asset id', async () => {
    mockApi([row(REAL)]);
    renderWithQuery(<HomeTopMovers />);
    await waitFor(() => {
      expect(screen.getAllByLabelText('Verified currency')).toHaveLength(1);
    });
  });

  it('does not badge a same-code asset from another issuer', async () => {
    mockApi([row(FAKE)]);
    renderWithQuery(<HomeTopMovers />);
    await waitFor(() => {
      expect(screen.getAllByText(/USDC/).length).toBeGreaterThan(0);
      expect(apiGet).toHaveBeenCalledWith('/v1/assets/verified');
    });
    expect(screen.queryByLabelText('Verified currency')).toBeNull();
  });
});
