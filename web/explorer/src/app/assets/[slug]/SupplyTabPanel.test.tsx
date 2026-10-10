import { afterEach, beforeEach, describe, it, expect, vi } from 'vitest';
import { render, screen, waitFor } from '@testing-library/react';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';

const supplyQuery = vi.hoisted(() => ({
  data: undefined as unknown,
}));

const assetExtra = vi.hoisted(() => ({
  fields: {} as Record<string, unknown>,
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
          ...assetExtra.fields,
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

afterEach(() => {
  assetExtra.fields = {};
});

// A failed /v1/chart must not assert "no history" about an asset the query
// never answered for.
describe('SupplyTabPanel market-cap chart', () => {
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

// Supply scales by the contract-declared decimals, not the asset's default,
// and the envelope's flags (stale, floor caveat) must reach the tab.
describe('SupplyTabPanel on-chain supply', () => {
  beforeEach(() => {
    vi.mocked(apiGet).mockResolvedValue({ data: { points: [] } });
  });

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

  it('shows the archived part of a storage-balance supply', () => {
    supplyQuery.data = {
      data: {
        ...storageSupply,
        decimals: 18,
        archived_balance_entries: 3,
        archived_balance_total: '250000000000000000000000',
      },
    };
    renderPanel();
    expect(screen.getByText('Archived')).toBeInTheDocument();
    expect(screen.getByText('250K')).toBeInTheDocument();
    expect(screen.getByText(/3 balances — TTL lapsed/)).toBeInTheDocument();
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
    expect(screen.getByText('≥ floor')).toHaveAttribute(
      'title',
      expect.stringContaining('A floor, not the exact supply'),
    );
    expect(screen.getByText(/Fresh to ledger 63,340,102/)).toBeInTheDocument();
  });

  it('renders the asset envelope flags', () => {
    supplyQuery.data = undefined;
    renderPanel();
    expect(screen.getByText('Reduced redundancy')).toBeInTheDocument();
  });

  it('compacts supplies exactly from their decimal strings', () => {
    // As floats both round up to 499.995 and render 500.
    const raw = '499994999999999999999';
    supplyQuery.data = {
      data: { ...storageSupply, total_supply: raw, decimals: 18 },
    };
    assetExtra.fields = { decimals: 18, circulating_supply: raw };
    renderPanel();
    expect(screen.getAllByText('499.99')).toHaveLength(2);
    expect(screen.queryByText('500')).not.toBeInTheDocument();
  });

  it('labels an issuer-declared max beside the circulating basis', () => {
    supplyQuery.data = undefined;
    assetExtra.fields = {
      max_supply: '5000000000',
      supply_basis: 'issuer_exclusion',
      max_supply_basis: 'sep1_declared_max',
    };
    renderPanel();
    expect(
      screen.getByText('Issuer-declared in stellar.toml'),
    ).toBeInTheDocument();
    expect(screen.getByText(/issuer_exclusion/)).toBeInTheDocument();
  });
});
