import { describe, it, expect, vi, beforeEach } from 'vitest';
import { render, screen, within } from '@testing-library/react';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';

vi.mock('@/api/client', async () => {
  const actual =
    await vi.importActual<typeof import('@/api/client')>('@/api/client');
  return { ...actual, apiGet: vi.fn() };
});

import { apiGet } from '@/api/client';
import { AssetOraclesPanel, spreadQuote } from './AssetOraclesPanel';

const USDC = 'USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN';
const FAKE_USDC =
  'USDC-GBNZILSTVQZ4R7IKQDGHYGY2QXL5QOFJYQMXPKWRRM5PAV7Y4M67AQUA';

// /v1/sources?class=oracle: coingecko is class=aggregator, so absent.
const ORACLE_SOURCES = ['band', 'chainlink', 'redstone', 'reflector-cex'].map(
  (name) => ({ name, class: 'oracle' }),
);

// Shapes from /v1/oracle/latest, which does NOT apply the class filter.
const reading = (
  source: string,
  price: string,
  decimals: number,
  over: Record<string, unknown> = {},
) => ({
  source,
  asset: 'crypto:USDC',
  quote: 'fiat:USD',
  ts: '2026-09-03T02:14:14Z',
  price,
  price_raw: price.replace('.', '').replace(/^0+/, ''),
  decimals,
  mapped: true,
  ...over,
});
const BAND = reading('band', '0.999822000', 9, {
  contract_id: 'CCQXWMZVM3KRTXTUPTN53YHL272QGKF32L7XEDNZ2S6OSUFK3NFBGG5M',
});
const REFLECTOR = reading('reflector-cex', '1.00003382630191', 14, {
  contract_id: 'CAFJZQWSED6YAWZU3GWRTOCNPPCGBN32L7QV43XX5LZLFTK6JLN34DLN',
  ts: '2026-09-03T02:55:00Z',
});
const COINGECKO = reading('coingecko', '0.86243500', 8, { quote: 'fiat:EUR' });
// An unmapped oracle symbol: reference-only, no canonical asset.
const RAW_USDC = reading('redstone', '0.99971949', 8, {
  asset: 'raw:USDC',
  mapped: false,
});

const COLLISION = {
  verified_slug: 'usdc',
  verified_asset_id: USDC,
  verified_name: 'USD Coin',
  verified_issuer: 'Circle (centre.io)',
  note: 'Exercise caution — this asset uses the ticker "USDC" but is not the verified USDC on Stellar.',
};

const ABSENT = 'No oracle publishes a price for this asset';
const UNAVAILABLE = /Oracle feeds unavailable right now/;

type Route = unknown[] | Error;

function mockApi({
  latest = [],
  streams = [],
  sources = ORACLE_SOURCES,
}: {
  latest?: Route;
  streams?: Route;
  sources?: Route;
}) {
  vi.mocked(apiGet).mockImplementation(async (path: string) => {
    const routes: Record<string, Route> = {
      '/v1/oracle/latest': latest,
      '/v1/oracle/streams': streams,
      '/v1/sources': sources,
    };
    const v = routes[path];
    if (v instanceof Error) throw v;
    // Anything else is AssetLink's SAC wrapper map.
    return { data: v ?? {} };
  });
}

function renderPanel(
  props: Partial<Parameters<typeof AssetOraclesPanel>[0]> = {},
) {
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  });
  return render(
    <QueryClientProvider client={client}>
      <AssetOraclesPanel assetID={USDC} symbol="USDC" {...props} />
    </QueryClientProvider>,
  );
}

type Text = string | RegExp;
const shows = (...t: Text[]) =>
  t.forEach((x) => expect(screen.getByText(x)).toBeInTheDocument());
const hides = (...t: Text[]) =>
  t.forEach((x) => expect(screen.queryByText(x)).not.toBeInTheDocument());

const pathsCalled = () =>
  vi.mocked(apiGet).mock.calls.map((c) => c[0] as string);

beforeEach(() => {
  vi.mocked(apiGet).mockReset();
});

describe('AssetOraclesPanel — verified asset', () => {
  it('lists each oracle with its reading, contract, scale and age', async () => {
    mockApi({ latest: [REFLECTOR, BAND] });
    renderPanel();
    await screen.findByText('Oracle feeds (2)');
    // Sorted by source. 6 decimals below 1, 4 at/above, so a depeg shows.
    const [band, refl] = screen.getAllByRole('row').slice(1);
    for (const t of ['band', '0.999822', '9', 'CCQX…GG5M'])
      within(band).getByText(t);
    for (const t of ['reflector-cex', '1.0000', '14'])
      within(refl).getByText(t);
    within(refl).getByRole('time', { hidden: true });
  });

  it.each([
    ['an aggregator row', COINGECKO, ['coingecko', '0.862435']],
    ['an unmapped raw: row', RAW_USDC, ['0.999719']],
  ])('keeps %s out of the attributed table', async (_, extra, absent) => {
    mockApi({ latest: [BAND, extra] });
    renderPanel();
    await screen.findByText('Oracle feeds (1)');
    shows('band');
    hides(...absent);
  });

  it('states the genuine absence when no oracle publishes the asset', async () => {
    mockApi({ latest: [] });
    renderPanel({ symbol: 'SHX' });
    expect(await screen.findByText(ABSENT)).toBeInTheDocument();
    shows(/publishes a reading for SHX/);
    hides(UNAVAILABLE);
  });

  // Only an answered-empty is ours to state as absence. The registry is
  // the panel's own coverage list, so an empty one is a failure too.
  it.each([
    ['/v1/oracle/latest fails', { latest: new Error('HTTP 503') }],
    [
      'the oracle-class registry fails',
      { latest: [BAND, COINGECKO], sources: new Error('HTTP 503') },
    ],
    [
      'the oracle-class registry answers empty',
      { latest: [BAND, REFLECTOR], sources: [] },
    ],
  ])('says unavailable, not absent, when %s', async (_, routes) => {
    mockApi(routes);
    renderPanel();
    expect(await screen.findByText(UNAVAILABLE)).toBeInTheDocument();
    hides(ABSENT, 'band', 'coingecko');
  });
});

describe('AssetOraclesPanel — unverified asset sharing a verified ticker', () => {
  it('requests no readings, says they are not attributed, and links the verified asset', async () => {
    mockApi({ latest: [BAND, REFLECTOR] });
    renderPanel({ assetID: FAKE_USDC, tickerCollision: COLLISION });
    await screen.findByText(/is not a coverage gap/i);
    shows(/declining to attribute another issuer's oracle prices/);
    shows(/a shared ticker is not a shared asset/);
    hides(ABSENT, /publishes a reading for USDC/, 'band', '0.999822');
    expect(pathsCalled()).not.toContain('/v1/oracle/latest');
    expect(screen.getByRole('link', { name: '/assets/usdc' })).toHaveAttribute(
      'href',
      '/assets/usdc',
    );
  });

  it('offers no link when the ticker has no verified Stellar issuance', async () => {
    mockApi({ latest: [] });
    renderPanel({
      assetID: 'XRP-GBXRPL45NPHCVMFFAYZVUVFFVKSIZ362ZXFP7I2ETNQ3QKZMFLPRDTD5',
      symbol: 'XRP',
      tickerCollision: {
        verified_slug: 'xrp',
        verified_asset_id: '',
        verified_name: 'XRP',
        note: 'Exercise caution …',
      },
    });
    await screen.findByText(
      /no Stellar asset may claim that ticker's readings/,
    );
    expect(screen.queryByRole('link', { name: /\/assets\/xrp/ })).toBeNull();
  });
});

describe('AssetOraclesPanel — symbol-matched raw: feeds', () => {
  it('are not fetched or shown by default', async () => {
    mockApi({ latest: [BAND], streams: [RAW_USDC] });
    renderPanel();
    await screen.findByText('Oracle feeds (1)');
    expect(pathsCalled()).not.toContain('/v1/oracle/streams');
    hides(/Unmapped feeds matching/);
  });

  it('render in their own group, unattributed, when enabled', async () => {
    mockApi({ latest: [BAND], streams: [RAW_USDC, COINGECKO] });
    renderPanel({ showSymbolMatchedRawFeeds: true });
    const group = (
      await screen.findByText('Unmapped feeds matching “USDC” (1)')
    ).closest('section') as HTMLElement;
    for (const t of [
      'redstone',
      '0.999719',
      'Symbol match only — not attributed to this asset',
    ]) {
      within(group).getByText(t);
    }
    const attributed = screen
      .getByText('Oracle feeds (1)')
      .closest('section') as HTMLElement;
    expect(within(attributed).queryByText('redstone')).not.toBeInTheDocument();
  });
});

describe('spreadQuote', () => {
  it('picks the quote two or more oracles share, else null', () => {
    const got = spreadQuote([BAND, REFLECTOR, COINGECKO]);
    expect(got?.quote).toBe('fiat:USD');
    expect(got?.rows.map((r) => r.source)).toEqual(['band', 'reflector-cex']);
    expect(spreadQuote([BAND, COINGECKO])).toBeNull();
  });

  it('plots each oracle on the USD band with its exact price', async () => {
    mockApi({ latest: [REFLECTOR, BAND] });
    renderPanel();
    const strip = await screen.findByLabelText(
      /Oracle spread vs USD, 2 oracles, spread/,
    );
    expect(strip.querySelector('[title^="band: "]')).not.toBeNull();
  });
});
