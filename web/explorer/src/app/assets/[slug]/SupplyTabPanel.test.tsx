import { describe, it, expect, vi } from 'vitest';
import { render, screen, waitFor } from '@testing-library/react';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';

vi.mock('@/api/client', async () => {
  const actual = await vi.importActual<typeof import('@/api/client')>('@/api/client');
  return { ...actual, apiGet: vi.fn() };
});

vi.mock('@/api/hooks', async () => {
  const actual = await vi.importActual<typeof import('@/api/hooks')>('@/api/hooks');
  return {
    ...actual,
    useAsset: () => ({
      data: {
        asset_id: 'USDC-GA5ZSEJ',
        code: 'USDC',
        decimals: 7,
        circulating_supply: '1000000000',
      },
      isLoading: false,
      isError: false,
    }),
    useAssetSupply: () => ({ data: undefined, isLoading: false, isError: false }),
  };
});

import { apiGet } from '@/api/client';
import { SupplyTabPanel } from './SupplyTabPanel';

// Frontend-honesty sweep: the market-cap chart had no isError branch, so
// a failed /v1/chart fell into `points.length < 2` and asserted "No
// market-cap history for this asset" — a claim about the asset made from
// a query that never answered.
describe('SupplyTabPanel market-cap chart', () => {
  function renderPanel() {
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    return render(
      <QueryClientProvider client={client}>
        <SupplyTabPanel assetID="USDC-GA5ZSEJ" />
      </QueryClientProvider>,
    );
  }

  it('says the history is unavailable when the chart query fails', async () => {
    vi.mocked(apiGet).mockRejectedValue(new Error('HTTP 503'));
    renderPanel();
    await waitFor(() =>
      expect(screen.getByText(/Market-cap history unavailable right now/)).toBeInTheDocument(),
    );
    expect(screen.queryByText(/No market-cap history for this asset/)).not.toBeInTheDocument();
  });

  it('keeps the genuine empty claim when the series is served but too short', async () => {
    vi.mocked(apiGet).mockResolvedValue({ data: { points: [] } });
    renderPanel();
    await waitFor(() =>
      expect(screen.getByText(/No market-cap history for this asset/)).toBeInTheDocument(),
    );
    expect(screen.queryByText(/Market-cap history unavailable/)).not.toBeInTheDocument();
  });
});
