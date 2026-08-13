import { describe, it, expect, vi, afterEach } from 'vitest';
import { render, screen } from '@testing-library/react';

import { LiquidityTabPanel } from './LiquidityTabPanel';

const ASSET = 'USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN';

function mockFetch(impl: () => Promise<Response>) {
  vi.stubGlobal('fetch', vi.fn(impl));
}

function jsonResponse(body: unknown, status = 200) {
  return new Response(JSON.stringify(body), {
    status,
    headers: { 'content-type': 'application/json' },
  });
}

async function renderPanel() {
  render(await LiquidityTabPanel({ assetID: ASSET, code: 'USDC' }));
}

// Frontend-honesty sweep: this build-time panel swallowed 5xx, 429 and
// its own 8s timeout into `[]` and then baked "No DEX pools observed
// touching USDC in the trailing 14 days" INTO THE STATIC EXPORT — the
// exact incident class src/lib/buildFetch.ts documents. Absent must read
// as unavailable; a served empty list keeps the honest empty claim.
describe('LiquidityTabPanel', () => {
  afterEach(() => {
    vi.unstubAllGlobals();
  });

  it('says the pool list is unavailable when the fetch fails', async () => {
    mockFetch(async () => {
      throw new Error('ETIMEDOUT');
    });
    await renderPanel();
    expect(screen.getByText(/Pool list unavailable for this build/)).toBeInTheDocument();
    expect(screen.queryByText(/No DEX pools observed touching/)).not.toBeInTheDocument();
  });

  it('says the pool list is unavailable on a 503', async () => {
    mockFetch(async () => jsonResponse({}, 503));
    await renderPanel();
    expect(screen.getByText(/Pool list unavailable for this build/)).toBeInTheDocument();
  });

  it('keeps the genuine empty claim when the API returns no pools', async () => {
    mockFetch(async () => jsonResponse({ data: [] }));
    await renderPanel();
    expect(screen.getByText(/No DEX pools observed touching/)).toBeInTheDocument();
    expect(screen.queryByText(/Pool list unavailable/)).not.toBeInTheDocument();
  });

  it('renders the pools when the API returns rows', async () => {
    mockFetch(async () =>
      jsonResponse({
        data: [
          {
            source: 'soroswap',
            base: ASSET,
            quote: 'native',
            last_price: '0.42',
            volume_24h_usd: '1000',
            trade_count_24h: 5,
          },
        ],
      }),
    );
    await renderPanel();
    expect(screen.getByText('soroswap')).toBeInTheDocument();
    expect(screen.queryByText(/Pool list unavailable/)).not.toBeInTheDocument();
    expect(screen.queryByText(/No DEX pools observed touching/)).not.toBeInTheDocument();
  });
});
