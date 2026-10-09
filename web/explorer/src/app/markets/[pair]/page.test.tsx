import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { render, screen } from '@testing-library/react';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';

vi.mock('@/lib/buildFetch', async () => {
  const actual =
    await vi.importActual<typeof import('@/lib/buildFetch')>(
      '@/lib/buildFetch',
    );
  return { ...actual, buildFetchData: vi.fn(), failBuild: vi.fn() };
});

// Client subcomponents unrelated to the 24h badge — stub them so this
// test only exercises the price panel's build-time data plumbing.
vi.mock('./OrderBookPanel', () => ({ OrderBookPanel: () => <div /> }));
vi.mock('./PairChart', () => ({ PairChart: () => <div /> }));
vi.mock('./SourceBreakdown', () => ({ SourceBreakdown: () => <div /> }));

import { buildFetchData } from '@/lib/buildFetch';
import PairPage from './page';

const BASE = 'crypto:XLM';
const QUOTE = 'fiat:USD';
const PAIR = `${BASE}~${QUOTE}`;

async function renderPair() {
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  });
  render(
    <QueryClientProvider client={client}>
      {await PairPage({ params: Promise.resolve({ pair: PAIR }) })}
    </QueryClientProvider>,
  );
}

// change_24h_pct is documented (openapi
// stellar-index.v1.yaml) as present on /v1/price/batch rows only and
// omitted on the single-row /v1/price response. The page baked its 24h
// badge off `price.change_24h_pct` — always undefined for the /v1/price
// fetch it actually makes — so the badge never rendered on a static
// build. It must instead be sourced from a /v1/price/batch call.
describe('markets/[pair]/page 24h change badge (CA2-A35-correct-5)', () => {
  beforeEach(() => {
    // Starve the client-side live change-summary worker so the badge
    // renders from the build-time value under test, not a live refresh.
    vi.stubGlobal('fetch', vi.fn().mockRejectedValue(new Error('offline')));
  });
  afterEach(() => {
    vi.unstubAllGlobals();
  });

  it('bakes the badge from /v1/price/batch, not the single-row /v1/price response', async () => {
    vi.mocked(buildFetchData).mockImplementation(async (path: string) => {
      if (path.startsWith('/v1/price/batch')) {
        return [{ asset_id: BASE, change_24h_pct: '+3.62' }] as never;
      }
      if (path.startsWith('/v1/price')) {
        // Single-row /v1/price never carries change_24h_pct per the
        // documented contract — this must NOT be where the badge comes
        // from.
        return {
          asset_id: BASE,
          quote: QUOTE,
          price: '0.204',
          price_type: 'vwap',
          observed_at: '2026-07-03T22:36:00Z',
        } as never;
      }
      return null;
    });

    await renderPair();

    expect(screen.getByText('+3.62%')).toBeInTheDocument();
  });

  it('renders no badge when the batch lookup has no comparison bucket', async () => {
    vi.mocked(buildFetchData).mockImplementation(async (path: string) => {
      if (path.startsWith('/v1/price/batch')) return [] as never;
      if (path.startsWith('/v1/price')) {
        return {
          asset_id: BASE,
          quote: QUOTE,
          price: '0.204',
          price_type: 'vwap',
          observed_at: '2026-07-03T22:36:00Z',
        } as never;
      }
      return null;
    });

    await renderPair();

    expect(screen.queryByText(/%/)).not.toBeInTheDocument();
  });
});

describe('markets/[pair]/page USD volume stats', () => {
  beforeEach(() => {
    vi.stubGlobal('fetch', vi.fn().mockRejectedValue(new Error('offline')));
  });
  afterEach(() => {
    vi.unstubAllGlobals();
  });

  function withPoints(vUsd: string[]) {
    vi.mocked(buildFetchData).mockImplementation(async (path: string) => {
      if (path.startsWith('/v1/chart')) {
        return {
          asset_id: BASE,
          points: vUsd.map((v, i) => ({
            t: `2026-01-01T0${i}:00:00Z`,
            p: '1',
            v_usd: v,
          })),
        } as never;
      }
      return null as never;
    });
  }

  function stat(label: string) {
    return screen.getByText(label, { selector: 'dt' }).nextElementSibling;
  }

  it('sums and formats the hourly volumes exactly', async () => {
    withPoints(['999.994999999999998', '0.000000000000001']);
    await renderPair();
    expect(stat('24h USD vol')).toHaveTextContent('$999.99');
    expect(stat('Last hour USD vol')).toHaveTextContent('<$0.01');
  });

  it('shows a million-dollar total in compact form', async () => {
    withPoints(['1234567', '1000']);
    await renderPair();
    expect(stat('24h USD vol')).toHaveTextContent('$1.24M');
    expect(stat('Last hour USD vol')).toHaveTextContent('$1K');
  });

  it.each(['1e5', '0', '-5'])('hides a 24h total of %s', async (v) => {
    withPoints([v]);
    await renderPair();
    expect(screen.queryByText('24h USD vol')).toBeNull();
  });
});
