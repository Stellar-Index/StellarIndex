import { describe, it, expect, vi, beforeEach } from 'vitest';
import { render, screen } from '@testing-library/react';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';

vi.mock('@/api/client', async () => {
  const actual =
    await vi.importActual<typeof import('@/api/client')>('@/api/client');
  return { ...actual, apiGet: vi.fn() };
});
vi.mock('next/navigation', () => ({
  useSearchParams: () => new URLSearchParams(),
}));

import { apiGet } from '@/api/client';
import { ContractView } from './ContractView';
import { SacAssetPanel } from './SacAssetPanel';

const USDC_SAC = 'CCW67TSZV3SSS2HXMBQ5JFGCKJNXKZM7UQUWUZPUTHXSTZLEO7SJMI75';
const XLM_SAC = 'CAS3J7GYLGXMF6TDJBBYYSE3HQ6BBSMLNUQ34T6TZMYMW2EVH34XOWMA';
const ISSUER = 'GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN';
const SAC_PROBLEM =
  '404 Not Found on /v1/contracts/x/wasm — Not Found: a Stellar Asset Contract [https://api.stellarindex.io/errors/contract-is-sac]';

const usdc = {
  asset_id: `USDC-${ISSUER}`,
  type: 'classic',
  code: 'USDC',
  issuer: ISSUER,
  decimals: 7,
  circulating_supply: '1234567890000000',
  total_supply: '2234567890000000',
  price_usd: '1.0001',
  market_cap_usd: '123456789',
};
const xlm = {
  asset_id: 'native',
  type: 'native',
  code: 'XLM',
  decimals: 7,
  circulating_supply: '500000000000000000',
};

function route(asset: Record<string, unknown>, wasmError?: string) {
  vi.mocked(apiGet).mockImplementation(async (p: string) => {
    const path = String(p);
    if (path.endsWith('/wasm')) throw new Error(wasmError ?? '404 other');
    if (path.endsWith('/holders'))
      return {
        data: {
          holder_count: 4321,
          holders: [{ account_id: ISSUER, balance: '50000000' }],
        },
      };
    if (path.endsWith('/supply'))
      return {
        data: {
          decimals: 7,
          source: 'mint_burn_flows',
          mint_total: '90000000',
          burn_total: '10000000',
        },
      };
    if (path.startsWith('/v1/assets/')) return { data: asset };
    if (path.startsWith('/v1/contracts/'))
      return { data: { contract_id: USDC_SAC, events: [] } };
    throw new Error(`unmocked ${path}`);
  });
}

function wrap(ui: React.ReactElement) {
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  });
  return render(
    <QueryClientProvider client={client}>{ui}</QueryClientProvider>,
  );
}

beforeEach(() => {
  vi.mocked(apiGet).mockReset();
});

describe('SacAssetPanel', () => {
  it('shows code, issuer, supply, price, market cap and holders', async () => {
    route(usdc);
    wrap(<SacAssetPanel contractId={USDC_SAC} />);
    expect(await screen.findByText('USDC')).toBeTruthy();
    expect(screen.getByTitle(ISSUER)).toBeTruthy();
    expect(screen.getByText('123.46M')).toBeTruthy();
    expect(screen.getByText('223.46M')).toBeTruthy();
    expect(screen.getByText('$123.46M')).toBeTruthy();
    expect(await screen.findByText('4.32K')).toBeTruthy();
    expect(await screen.findByTestId('supply-flows-bar')).toBeTruthy();
    expect(screen.getByText(/View all on the asset page/)).toBeTruthy();
  });

  it('renders native XLM without an issuer', async () => {
    route(xlm);
    wrap(<SacAssetPanel contractId={XLM_SAC} />);
    expect(await screen.findByText('XLM')).toBeTruthy();
    expect(screen.getByText('Native asset')).toBeTruthy();
    expect(screen.queryByText(/^Issuer/)).toBeNull();
    expect(screen.queryByText('Price')).toBeNull();
  });
});

describe('ContractView SAC detection', () => {
  it('shows the wrapped-asset panel when the wasm problem type is contract-is-sac', async () => {
    route(usdc, SAC_PROBLEM);
    wrap(<ContractView id={USDC_SAC} />);
    expect(await screen.findByTestId('sac-asset-panel')).toBeTruthy();
  });

  it('ignores prose: a detail mentioning SACs without the type shows no panel', async () => {
    route(usdc, '404 Not Found — a Stellar Asset Contract');
    wrap(<ContractView id={USDC_SAC} />);
    await screen.findByText(/Code \(WASM\)/);
    expect(screen.queryByTestId('sac-asset-panel')).toBeNull();
  });

  it('shows no panel for a regular wasm contract', async () => {
    route(usdc, '404 Not Found — not captured');
    wrap(<ContractView id={USDC_SAC} />);
    await screen.findByText(/Code \(WASM\)/);
    expect(screen.queryByTestId('sac-asset-panel')).toBeNull();
  });
});
