import { describe, it, expect, vi, beforeEach } from 'vitest';
import { render, screen, waitFor, within } from '@testing-library/react';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';

vi.mock('@/api/client', async () => {
  const actual =
    await vi.importActual<typeof import('@/api/client')>('@/api/client');
  return { ...actual, apiGet: vi.fn() };
});

import { apiGet } from '@/api/client';
import { AssetOraclesPanel } from './AssetOraclesPanel';

const USDC = 'USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN';
const FAKE_USDC =
  'USDC-GBNZILSTVQZ4R7IKQDGHYGY2QXL5QOFJYQMXPKWRRM5PAV7Y4M67AQUA';

// The oracle-class registry as /v1/sources?class=oracle serves it (r1,
// 2026-09-03). coingecko is class=aggregator and is therefore ABSENT —
// which is the whole point of the filter under test.
const ORACLE_SOURCES = [
  { name: 'band', class: 'oracle' },
  { name: 'chainlink', class: 'oracle' },
  { name: 'redstone', class: 'oracle' },
  { name: 'reflector-cex', class: 'oracle' },
];

// Verbatim shapes from GET /v1/oracle/latest?asset=USDC-GA5Z… on r1
// (2026-09-03), including the `coingecko` row the endpoint really does
// return: /v1/oracle/latest does NOT apply the class=oracle filter that
// /v1/oracle/streams applies.
const BAND = {
  source: 'band',
  contract_id: 'CCQXWMZVM3KRTXTUPTN53YHL272QGKF32L7XEDNZ2S6OSUFK3NFBGG5M',
  asset: 'crypto:USDC',
  quote: 'fiat:USD',
  ts: '2026-09-03T02:14:14Z',
  price: '0.999822000',
  price_raw: '999822000',
  decimals: 9,
  observer: 'GB4KMSLYVTWEEWC4QHSIXJXGKS7X6QQ5NO6NKDV5BS34OZRA4X2HKZCU',
  mapped: true,
};
const REFLECTOR = {
  source: 'reflector-cex',
  contract_id: 'CAFJZQWSED6YAWZU3GWRTOCNPPCGBN32L7QV43XX5LZLFTK6JLN34DLN',
  asset: 'crypto:USDC',
  quote: 'fiat:USD',
  ts: '2026-09-03T02:55:00Z',
  price: '1.00003382630191',
  price_raw: '100003382630191',
  decimals: 14,
  mapped: true,
};
const COINGECKO = {
  source: 'coingecko',
  asset: 'crypto:USDC',
  quote: 'fiat:EUR',
  ts: '2026-09-03T02:57:37Z',
  price: '0.86243500',
  price_raw: '86243500',
  decimals: 8,
  mapped: true,
};
// An unmapped oracle symbol — reference-only, maps to no canonical asset.
const RAW_USDC = {
  source: 'redstone',
  contract_id: 'CA526Y2NQWGWVVQ7RFFPGAZMU66PSYJ3UC2MTVAV4ZU7OM5BOPHDXUSG',
  asset: 'raw:USDC',
  quote: 'fiat:USD',
  ts: '2026-09-03T02:30:08Z',
  price: '0.99971949',
  price_raw: '99971949',
  decimals: 8,
  mapped: false,
};

const COLLISION = {
  verified_slug: 'usdc',
  verified_asset_id: USDC,
  verified_name: 'USD Coin',
  verified_issuer: 'Circle (centre.io)',
  note: 'Exercise caution — this asset uses the ticker "USDC" but is not the verified USDC on Stellar.',
};

type Routes = {
  latest?: unknown[] | Error;
  streams?: unknown[] | Error;
  sources?: unknown[] | Error;
};

function mockApi({
  latest = [],
  streams = [],
  sources = ORACLE_SOURCES,
}: Routes) {
  vi.mocked(apiGet).mockImplementation(async (path: string) => {
    const pick = (v: unknown[] | Error) => {
      if (v instanceof Error) throw v;
      return { data: v };
    };
    if (path === '/v1/oracle/latest') return pick(latest);
    if (path === '/v1/oracle/streams') return pick(streams);
    if (path === '/v1/sources') return pick(sources);
    // AssetLink's SAC wrapper map.
    return { data: {} };
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

function pathsCalled(): string[] {
  return vi.mocked(apiGet).mock.calls.map((c) => c[0] as string);
}

describe('AssetOraclesPanel', () => {
  beforeEach(() => {
    vi.mocked(apiGet).mockReset();
  });

  describe('a verified asset', () => {
    it('lists each oracle with its reading, contract, scale and age', async () => {
      mockApi({ latest: [REFLECTOR, BAND] });
      renderPanel();

      await waitFor(() =>
        expect(screen.getByText('Oracle feeds (2)')).toBeInTheDocument(),
      );
      // Alphabetical by source, as the table sorts.
      const rows = screen.getAllByRole('row').slice(1);
      expect(within(rows[0]).getByText('band')).toBeInTheDocument();
      expect(within(rows[1]).getByText('reflector-cex')).toBeInTheDocument();

      // The corrected VALUES, not merely "something rendered": the
      // formatter keeps 4 decimals at/above 1 and 6 below it, so a
      // stablecoin's departure from the peg stays visible.
      expect(within(rows[0]).getByText('0.999822')).toBeInTheDocument();
      expect(within(rows[1]).getByText('1.0000')).toBeInTheDocument();
      // Declared scale (ADR-0003 — price_raw is meaningless without it).
      expect(within(rows[0]).getByText('9')).toBeInTheDocument();
      expect(within(rows[1]).getByText('14')).toBeInTheDocument();
      // Publishing contract identity, truncated but copyable.
      expect(within(rows[0]).getByText('CCQX…GG5M')).toBeInTheDocument();
      // Age of the reading.
      expect(
        within(rows[1]).getByRole('time', { hidden: true }) ??
          within(rows[1]).getByText(/ago|now/),
      ).toBeTruthy();
    });

    // /v1/oracle/latest really does return a coingecko row; coingecko is
    // class=aggregator. A panel headed "oracle feeds" that lists it says
    // something false about what CoinGecko is.
    it('excludes an aggregator row that /v1/oracle/latest returned', async () => {
      mockApi({ latest: [BAND, COINGECKO] });
      renderPanel();

      await waitFor(() =>
        expect(screen.getByText('Oracle feeds (1)')).toBeInTheDocument(),
      );
      expect(screen.getByText('band')).toBeInTheDocument();
      expect(screen.queryByText('coingecko')).not.toBeInTheDocument();
      // …and the aggregator's EUR reading is not smuggled in unlabelled.
      expect(screen.queryByText('0.862435')).not.toBeInTheDocument();
    });

    it('never lets an unmapped raw: row join the attributed table', async () => {
      mockApi({ latest: [BAND, RAW_USDC] });
      renderPanel();

      await waitFor(() =>
        expect(screen.getByText('Oracle feeds (1)')).toBeInTheDocument(),
      );
      expect(screen.getByText('band')).toBeInTheDocument();
      expect(screen.queryByText('0.999719')).not.toBeInTheDocument();
    });

    it('states the genuine absence when no oracle publishes the asset', async () => {
      mockApi({ latest: [] });
      renderPanel({ symbol: 'SHX' });

      await waitFor(() =>
        expect(
          screen.getByText('No oracle publishes a price for this asset'),
        ).toBeInTheDocument(),
      );
      expect(
        screen.getByText(/publishes a reading for SHX/),
      ).toBeInTheDocument();
      expect(
        screen.queryByText(/unavailable right now/),
      ).not.toBeInTheDocument();
    });

    // Absent (the request never answered) is not empty (it answered with
    // nothing) — only the second is ours to state as a fact.
    it('says unavailable, not absent, when /v1/oracle/latest fails', async () => {
      mockApi({ latest: new Error('HTTP 503') });
      renderPanel();

      await waitFor(() =>
        expect(
          screen.getByText(/Oracle feeds unavailable right now/),
        ).toBeInTheDocument(),
      );
      expect(
        screen.queryByText('No oracle publishes a price for this asset'),
      ).not.toBeInTheDocument();
    });

    // Without the registry we cannot tell an oracle from an aggregator,
    // so the honest render is "unavailable", not an unclassified list.
    it('says unavailable when the oracle-class registry fails', async () => {
      mockApi({ latest: [BAND, COINGECKO], sources: new Error('HTTP 503') });
      renderPanel();

      await waitFor(() =>
        expect(
          screen.getByText(/Oracle feeds unavailable right now/),
        ).toBeInTheDocument(),
      );
      expect(screen.queryByText('coingecko')).not.toBeInTheDocument();
      expect(screen.queryByText('band')).not.toBeInTheDocument();
    });
  });

  // The #336 gate: an asset that merely BORROWS a verified currency's
  // ticker is served no oracle rows, and the reason must not read as a
  // coverage gap.
  describe('an unverified asset sharing a verified ticker', () => {
    it('says the readings are not attributed, not that coverage is missing', async () => {
      mockApi({ latest: [] });
      renderPanel({ assetID: FAKE_USDC, tickerCollision: COLLISION });

      await waitFor(() =>
        expect(screen.getByText(/is not a coverage gap/i)).toBeInTheDocument(),
      );
      expect(
        screen.getByText(
          /declining to attribute another issuer's oracle prices/,
        ),
      ).toBeInTheDocument();
      expect(
        screen.getByText(/a shared ticker is not a shared asset/),
      ).toBeInTheDocument();
      // The sentence this state exists to avoid.
      expect(
        screen.queryByText('No oracle publishes a price for this asset'),
      ).not.toBeInTheDocument();
      expect(
        screen.queryByText(/publishes a reading for USDC/),
      ).not.toBeInTheDocument();
      // And the reader is pointed at the asset that DOES own the ticker.
      expect(
        screen.getByRole('link', { name: '/assets/usdc' }),
      ).toHaveAttribute('href', '/assets/usdc');
    });

    // Belt-and-braces on the server gate: the request is not made at all,
    // so a regressed API cannot put Circle's prices under this asset.
    it('does not request oracle readings for it, and renders none if served', async () => {
      mockApi({ latest: [BAND, REFLECTOR] });
      renderPanel({ assetID: FAKE_USDC, tickerCollision: COLLISION });

      await waitFor(() =>
        expect(screen.getByText(/is not a coverage gap/i)).toBeInTheDocument(),
      );
      expect(pathsCalled()).not.toContain('/v1/oracle/latest');
      expect(screen.queryByText('band')).not.toBeInTheDocument();
      expect(screen.queryByText('reflector-cex')).not.toBeInTheDocument();
      expect(screen.queryByText('0.999822')).not.toBeInTheDocument();
    });

    // A ticker whose verified currency has NO Stellar issuance (USDT,
    // XRP, BTC): there is nothing to link to, so do not fabricate a link.
    it('does not offer a link when the ticker has no verified Stellar issuance', async () => {
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

      await waitFor(() =>
        expect(
          screen.getByText(/No verified/, { exact: false }),
        ).toBeInTheDocument(),
      );
      expect(
        screen.getByText(/no Stellar asset may claim that ticker's readings/),
      ).toBeInTheDocument();
      expect(screen.queryByRole('link', { name: /\/assets\/xrp/ })).toBeNull();
    });
  });

  // Ash's call (see SHOW_SYMBOL_MATCHED_RAW_FEEDS): symbol-matched raw
  // feeds are OFF by default, and turning them on is one prop.
  describe('symbol-matched raw: feeds', () => {
    it('are not fetched or shown by default', async () => {
      mockApi({ latest: [BAND], streams: [RAW_USDC] });
      renderPanel();

      await waitFor(() =>
        expect(screen.getByText('Oracle feeds (1)')).toBeInTheDocument(),
      );
      expect(pathsCalled()).not.toContain('/v1/oracle/streams');
      expect(
        screen.queryByText(/Unmapped feeds matching/),
      ).not.toBeInTheDocument();
    });

    it('render in their own group, unattributed, when enabled', async () => {
      mockApi({ latest: [BAND], streams: [RAW_USDC, COINGECKO] });
      renderPanel({ showSymbolMatchedRawFeeds: true });

      await waitFor(() =>
        expect(
          screen.getByText('Unmapped feeds matching “USDC” (1)'),
        ).toBeInTheDocument(),
      );
      const group = screen
        .getByText('Unmapped feeds matching “USDC” (1)')
        .closest('section') as HTMLElement;
      expect(within(group).getByText('redstone')).toBeInTheDocument();
      expect(within(group).getByText('0.999719')).toBeInTheDocument();
      expect(
        within(group).getByText(/that is a resemblance, not an identity/),
      ).toBeInTheDocument();
      // Still absent from the attributed table above.
      const attributed = screen
        .getByText('Oracle feeds (1)')
        .closest('section') as HTMLElement;
      expect(
        within(attributed).queryByText('redstone'),
      ).not.toBeInTheDocument();
    });
  });
});
