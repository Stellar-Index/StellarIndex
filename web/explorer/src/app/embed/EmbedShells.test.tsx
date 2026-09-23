import { afterEach, describe, expect, it, vi } from 'vitest';
import { render, screen, waitFor } from '@testing-library/react';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import type { ReactNode } from 'react';

import EmbedAssetPage, {
  generateStaticParams as assetParams,
} from './asset/[slug]/page';
import EmbedCurrencyPage, {
  generateStaticParams as currencyParams,
} from './currency/[ticker]/page';
import EmbedPairPage, {
  generateStaticParams as pairParams,
} from './pair/[pair]/page';
import { EmbedAssetPathView } from './asset/[slug]/EmbedAssetPathView';
import { EmbedCurrencyPathView } from './currency/[ticker]/EmbedCurrencyPathView';
import { EmbedPairPathView } from './pair/[pair]/EmbedPairPathView';

// T291: functions/embed/{asset,currency,pair}/[[path]].js serve
// /embed/<kind>/shell/ for any id outside the build's pre-render. That
// document must be baked on every generateStaticParams path, must not
// read the API at build time, and must render the widget the URL names.

function jsonResponse(body: unknown, status = 200) {
  return new Response(JSON.stringify(body), {
    status,
    headers: { 'content-type': 'application/json' },
  });
}

function stubFetch(route: (url: string) => Response) {
  const spy = vi.fn(async (input: RequestInfo | URL) => route(String(input)));
  vi.stubGlobal('fetch', spy);
  return spy;
}

function renderAt(pathname: string, node: ReactNode) {
  window.history.pushState({}, '', pathname);
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  });
  render(<QueryClientProvider client={client}>{node}</QueryClientProvider>);
}

const priceCalls = (spy: ReturnType<typeof stubFetch>) =>
  spy.mock.calls
    .map(([u]) => String(u))
    .filter((u) => u.includes('/v1/price?'));

afterEach(() => {
  vi.unstubAllGlobals();
});

describe('embed shells are baked on every build path', () => {
  it('when the listings answer', async () => {
    stubFetch((url) => {
      if (url.includes('/v1/markets')) {
        return jsonResponse({ data: [{ base: 'native', quote: 'USDC-GX' }] });
      }
      if (url.includes('/v1/assets/verified')) {
        return jsonResponse({ data: [{ ticker: 'EUR', class: 'fiat' }] });
      }
      return jsonResponse({ data: [{ slug: 'aqua', asset_id: 'AQUA-GX' }] });
    });
    expect(await assetParams()).toContainEqual({ slug: 'shell' });
    expect(await currencyParams()).toEqual([
      { ticker: 'EUR' },
      { ticker: 'shell' },
    ]);
    expect(await pairParams()).toEqual([
      { pair: 'native~USDC-GX' },
      { pair: 'shell' },
    ]);
  });

  it('when the listings are unreachable', async () => {
    stubFetch(() => jsonResponse({}, 503));
    expect(await currencyParams()).toContainEqual({ ticker: 'shell' });
    expect(await pairParams()).toContainEqual({ pair: 'shell' });
  });
});

describe('embed shell pages render without a build-time read', () => {
  it.each([
    [
      'asset',
      () => EmbedAssetPage({ params: Promise.resolve({ slug: 'shell' }) }),
    ],
    [
      'currency',
      () => EmbedCurrencyPage({ params: Promise.resolve({ ticker: 'shell' }) }),
    ],
    [
      'pair',
      () => EmbedPairPage({ params: Promise.resolve({ pair: 'shell' }) }),
    ],
  ])('%s', async (_kind, renderPage) => {
    const spy = stubFetch(() => jsonResponse({}, 404));
    await renderPage();
    expect(spy).not.toHaveBeenCalled();
  });
});

describe('embed path views render the id the URL names', () => {
  it('pair: prices the base/quote from the URL', async () => {
    const spy = stubFetch(() => jsonResponse({ data: { price: '0.5' } }));
    renderAt('/embed/pair/native~EURC-GNEWISSUER/', <EmbedPairPathView />);
    await waitFor(() => expect(priceCalls(spy)).toHaveLength(1));
    const url = new URL(priceCalls(spy)[0]);
    expect(url.searchParams.get('asset')).toBe('native');
    expect(url.searchParams.get('quote')).toBe('EURC-GNEWISSUER');
    expect(await screen.findByText('0.500000')).toBeInTheDocument();
  });

  it('pair: rejects a slug with no separator', () => {
    stubFetch(() => jsonResponse({}, 404));
    renderAt('/embed/pair/not-a-pair/', <EmbedPairPathView />);
    expect(screen.getByText('Invalid pair slug')).toBeInTheDocument();
  });

  it('currency: upper-cases a hand-typed lowercase ticker', async () => {
    const spy = stubFetch(() => jsonResponse({ data: { price: '1.1' } }));
    renderAt('/embed/currency/eur/', <EmbedCurrencyPathView />);
    await waitFor(() => expect(priceCalls(spy)).toHaveLength(1));
    expect(new URL(priceCalls(spy)[0]).searchParams.get('asset')).toBe(
      'fiat:EUR',
    );
    expect(await screen.findByText('$1.1000')).toBeInTheDocument();
  });

  it('asset: resolves the slug to its canonical id for the live price', async () => {
    const spy = stubFetch((url) =>
      url.includes('/v1/price?')
        ? jsonResponse({ data: { price: '2' } })
        : jsonResponse({
            data: {
              asset_id: 'NEWT-GNEWISSUER',
              code: 'NEWT',
              price_usd: '1.5',
            },
          }),
    );
    renderAt('/embed/asset/newt/', <EmbedAssetPathView />);
    expect(await screen.findByText('NEWT')).toBeInTheDocument();
    expect(String(spy.mock.calls[0][0])).toContain('/v1/assets/newt');
    await waitFor(() => expect(priceCalls(spy)).toHaveLength(1));
    expect(new URL(priceCalls(spy)[0]).searchParams.get('asset')).toBe(
      'NEWT-GNEWISSUER',
    );
  });

  it('asset: says so when the API does not know the slug', async () => {
    stubFetch(() => jsonResponse({}, 404));
    renderAt('/embed/asset/nope/', <EmbedAssetPathView />);
    expect(await screen.findByText(/No data for nope/)).toBeInTheDocument();
  });
});
