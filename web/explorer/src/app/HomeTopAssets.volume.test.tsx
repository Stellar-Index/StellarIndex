import { describe, it, expect, vi, beforeEach } from 'vitest';
import { render, screen, waitFor } from '@testing-library/react';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { CURRENT_NETWORK } from '@/lib/networks';

vi.mock('@/api/client', async () => {
  const actual =
    await vi.importActual<typeof import('@/api/client')>('@/api/client');
  return { ...actual, apiGet: vi.fn() };
});

import { apiGet } from '@/api/client';
import { HomeTopAssets } from './HomeTopAssets';

const ISSUER = 'GBLTXF46JTCGMWFJASQLVXMMA36IPYTDCN4EN73HRXCGDCGYBZM3A6VD';
const ROW = {
  kind: 'stellar_asset',
  asset_id: `VOL-${ISSUER}`,
  type: 'classic',
  code: 'VOL',
  decimals: 7,
  sep1_status: 'not_applicable',
  volume_24h_usd: '999.994999999999998',
  observation_count: 10,
};

describe('HomeTopAssets 24h volume cell', () => {
  beforeEach(() => {
    vi.mocked(apiGet).mockReset();
    vi.mocked(apiGet).mockImplementation(async (path: string) => {
      if (path.startsWith('/v1/assets/verified')) return { data: [] };
      if (path.startsWith('/v1/assets/native')) return { data: null };
      if (path.startsWith('/v1/assets')) return { data: [ROW] };
      return { data: [] };
    });
  });

  it.runIf(CURRENT_NETWORK.pricing)(
    'formats the decimal string exactly',
    async () => {
      const qc = new QueryClient({
        defaultOptions: { queries: { retry: false, gcTime: 0 } },
      });
      render(
        <QueryClientProvider client={qc}>
          <HomeTopAssets />
        </QueryClientProvider>,
      );
      await waitFor(() => {
        expect(screen.getByText('$999.99')).toBeInTheDocument();
      });
    },
  );
});
