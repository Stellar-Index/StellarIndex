import { describe, it, expect, vi } from 'vitest';
import { render, screen, waitFor } from '@testing-library/react';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';

const supplyQuery = vi.hoisted(() => ({
  data: undefined as unknown,
}));

vi.mock('@/api/client', async () => {
  const actual =
    await vi.importActual<typeof import('@/api/client')>('@/api/client');
  return { ...actual, apiGet: vi.fn() };
});

vi.mock('@/api/hooks', async () => {
  const actual =
    await vi.importActual<typeof import('@/api/hooks')>('@/api/hooks');
  return {
    ...actual,
    useAsset: () => ({
      data: {
        data: {
          asset_id: 'USDC-GA5ZSEJ',
          code: 'USDC',
          decimals: 7,
          circulating_supply: '1000000000',
        },
        flags: { reduced_redundancy: true },
      },
      isLoading: false,
      isError: false,
    }),
    useAssetSupply: () => ({
      data: supplyQuery.data,
      isLoading: false,
      isError: false,
    }),
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
    const client = new QueryClient({
      defaultOptions: { queries: { retry: false } },
    });
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
      expect(
        screen.getByText(/Market-cap history unavailable right now/),
      ).toBeInTheDocument(),
    );
    expect(
      screen.queryByText(/No market-cap history for this asset/),
    ).not.toBeInTheDocument();
  });

  it('keeps the genuine empty claim when the series is served but too short', async () => {
    vi.mocked(apiGet).mockResolvedValue({ data: { points: [] } });
    renderPanel();
    await waitFor(() =>
      expect(
        screen.getByText(/No market-cap history for this asset/),
      ).toBeInTheDocument(),
    );
    expect(
      screen.queryByText(/Market-cap history unavailable/),
    ).not.toBeInTheDocument();
  });
});

// #1013: the live supply block scaled every token by the ASSET's decimals
// (7 by default), so an 18-decimal token read 10^11 too large, and the
// hook dropped the envelope so `flags.stale` and the floor caveat never
// rendered.
describe('SupplyTabPanel on-chain supply', () => {
  function renderPanel() {
    vi.mocked(apiGet).mockResolvedValue({ data: { points: [] } });
    const client = new QueryClient({
      defaultOptions: { queries: { retry: false } },
    });
    return render(
      <QueryClientProvider client={client}>
        <SupplyTabPanel assetID="USDC-GA5ZSEJ" />
      </QueryClientProvider>,
    );
  }

  const storageSupply = {
    asset_id: 'USDC-GA5ZSEJ',
    total_supply: '1000000000000000000000000',
    flow_count: 0,
    source: 'contract_storage_balances',
    balance_entries: 42,
  };

  it('scales by the contract-declared decimals, not the asset default', () => {
    supplyQuery.data = { data: { ...storageSupply, decimals: 18 } };
    renderPanel();
    expect(screen.getByText('1M')).toBeInTheDocument();
    expect(screen.queryByText('100000T')).not.toBeInTheDocument();
  });

  it('renders base units when the contract declares no scale', () => {
    supplyQuery.data = { data: storageSupply };
    renderPanel();
    expect(
      screen.getByText('1,000,000,000,000,000,000,000,000'),
    ).toBeInTheDocument();
    expect(screen.getByText(/declares no scale/)).toBeInTheDocument();
    expect(screen.queryByText('100000T')).not.toBeInTheDocument();
  });

  it('renders the envelope stale flag and the lower-bound floor', () => {
    supplyQuery.data = {
      data: {
        ...storageSupply,
        decimals: 18,
        circulating_supply_lower_bound: true,
        supply_consistent: false,
        as_of_ledger: 63340102,
      },
      flags: { stale: true },
    };
    renderPanel();
    expect(screen.getByText('≥ 1M')).toBeInTheDocument();
    expect(screen.getByText('Stale')).toBeInTheDocument();
    expect(
      screen.getByText(/A floor, not the exact supply/),
    ).toBeInTheDocument();
    expect(screen.getByText(/Fresh to ledger 63,340,102/)).toBeInTheDocument();
  });

  // #660: useAsset dropped the envelope, so the asset's own flags never
  // reached the client-fetched Supply tab.
  it('renders the asset envelope flags', () => {
    supplyQuery.data = undefined;
    renderPanel();
    expect(screen.getByText('Reduced redundancy')).toBeInTheDocument();
  });
});
