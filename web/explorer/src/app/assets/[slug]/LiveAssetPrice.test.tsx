import { render, screen } from '@testing-library/react';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import type { ComponentProps } from 'react';

import type { LiveTip, StreamFrame } from '@/lib/live/hooks';

import { LiveAssetPrice } from './LiveAssetPrice';

const useTipStream = vi.hoisted(() =>
  vi.fn<
    (asset: string | null, quote?: string) => StreamFrame<LiveTip> | null
  >(),
);
vi.mock('@/lib/live/hooks', async (importOriginal) => ({
  ...(await importOriginal<typeof import('@/lib/live/hooks')>()),
  useTipStream,
  // A real clock so staleness verdicts run inside render.
  useLiveClock: () => Date.now(),
}));

const AUDD = 'AUDD-GDC7X2MXTYSAKUUGAIQ7J7RPEIM7GXSAIWFYWWH4GLNFECQVJJLB2EEU';
const USDC = 'USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN';
const THIN = 'THIN-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN';
const CAUP7 = 'CAUP7NFABXE5TJRL3FKTPMWRLC7IAXYDCTHQRFSCLR5TMGKHOOQO772J';
const PEG_CAPTION =
  /pegged · declared 1:1 fiat peg × fx rate · not a market price/i;
const WITHHELD = 'https://api.stellarindex.io/errors/price-withheld';

type Text = string | RegExp;
const shows = (...t: Text[]) =>
  t.forEach((x) => expect(screen.getByText(x)).toBeInTheDocument());
const hides = (...t: Text[]) =>
  t.forEach((x) => expect(screen.queryByText(x)).not.toBeInTheDocument());

type Props = ComponentProps<typeof LiveAssetPrice>;

function renderPrice(over: Partial<Props> = {}) {
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  });
  const props: Props = {
    assetID: 'native',
    initialPrice: '0.17',
    initialProvenance: 'vwap1m',
    ...over,
  };
  return render(
    <QueryClientProvider client={client}>
      <LiveAssetPrice {...props} />
    </QueryClientProvider>,
  );
}

// Requests whose URL lacks `path` get a bodiless 404.
function stubFetch(status: number, body: unknown, path = '') {
  const reply = async (input: string | URL) => {
    const hit = String(input).includes(path);
    const code = hit ? status : 404;
    return {
      status: code,
      ok: code === 200,
      json: async () => (hit ? body : {}),
    };
  };
  vi.stubGlobal('fetch', vi.fn().mockImplementation(reply));
}

const fetchSettled = () => vi.waitFor(() => expect(fetch).toHaveBeenCalled());

beforeEach(() => {
  useTipStream.mockReturnValue(null);
  // Both the /v1/price poll and the /v1/changes query fail unless a test
  // stubs its own fetch, so baked + stream behaviour is deterministic.
  vi.stubGlobal('fetch', vi.fn().mockRejectedValue(new Error('offline')));
});
afterEach(() => {
  useTipStream.mockReset();
  vi.unstubAllGlobals();
});

describe('LiveAssetPrice', () => {
  it.each([
    ['vwap1m', 'native', '0.17', /\$0\.17/, /1-min VWAP · USD/i, null],
    ['declared_peg', AUDD, '0.655', /\$0\.655/, PEG_CAPTION, /VWAP/i],
    // Fetched live from /v1/assets, and not the aggregator's FX cross-rate.
    [
      'transitive',
      CAUP7,
      '7768.93',
      /7,?768/,
      /two-hop DEX route/i,
      /as baked at deploy|triangulated via XLM/i,
    ],
  ] as const)(
    'renders a baked %s price with its own caption',
    (initialProvenance, assetID, initialPrice, price, caption, absent) => {
      renderPrice({ assetID, initialPrice, initialProvenance });
      shows(price, caption);
      if (absent) hides(absent);
      expect(
        screen.queryByRole('status', { name: 'live' }),
      ).not.toBeInTheDocument();
    },
  );

  it.each([
    ['a fresh tip frame takes over with the live caption', 0, '0.1745', true],
    ['a stale tip frame does not claim live', 60_000, '0.17', false],
  ])('%s', (_, age, shown, live) => {
    useTipStream.mockReturnValue({
      data: {
        data: { price: '0.1745' },
        as_of: '2026-08-08T00:00:00Z',
        sources: ['sdex'],
      },
      receivedAt: Date.now() - age,
    });
    renderPrice();
    expect(
      screen.getByText(new RegExp(`\\$${shown.replace('.', '\\.')}`)),
    ).toBeInTheDocument();
    expect(
      screen.queryByText(/live tip price · USD · streaming/i) !== null,
    ).toBe(live);
    expect(screen.queryByRole('status', { name: 'live' }) !== null).toBe(live);
  });

  // Withheld-replaces-baked purges market snapshots; a peg or a
  // global-market fill is not one, so the verdict must not blank it.
  it.each([
    ['declared_peg', AUDD, '0.655', PEG_CAPTION],
    [
      'global_market',
      USDC,
      '0.97',
      /global market · cross-venue aggregator price/i,
    ],
  ] as const)(
    'a price-withheld poll does not blank a %s price',
    async (initialProvenance, assetID, initialPrice, caption) => {
      stubFetch(404, { type: WITHHELD });
      renderPrice({ assetID, initialPrice, initialProvenance });
      await fetchSettled();
      await screen.findByText(`$${initialPrice}`, { exact: false });
      shows(caption);
      hides(/price withheld|as baked at deploy/i);
    },
  );

  it('a price-withheld poll replaces a market-provenance price with the server wording', async () => {
    stubFetch(404, {
      type: WITHHELD,
      title: 'Price withheld — issuer flagged',
      detail:
        'a directory-flagged issuer is on one leg of native / fiat:USD, so no price is published for this market',
    });
    renderPrice({ initialProvenance: 'listing' });
    await screen.findByText(/directory-flagged issuer/i);
    hides(/market too thin to aggregate/i, /\$0\.17/);
  });

  it('a thin-market poll shows the price with a warning badge and the substance note', async () => {
    stubFetch(200, {
      data: {
        price: '0.0421',
        substance: {
          base: THIN,
          quote: 'fiat:USD',
          window_seconds: 86400,
          measured_at: '2026-10-07T00:00:00Z',
          volume_usd: '12.5',
          buckets: 2,
          valued_buckets: 2,
          span_seconds: 600,
          floor: {
            min_volume_usd: '1000',
            min_buckets: 6,
            min_span_seconds: 3600,
          },
          failed: 'volume',
        },
      },
      flags: { thin_market: true },
    });
    renderPrice({ assetID: THIN, initialPrice: null, initialProvenance: null });
    expect(await screen.findByText(/\$0\.0421/)).toBeInTheDocument();
    expect(String(vi.mocked(fetch).mock.calls[0]?.[0])).toContain(
      'include_thin=true',
    );
    shows(/thin market · low confidence · in no total/i);
    hides(/1-min VWAP/i);
    expect(screen.getByText('⚠').getAttribute('title')).toMatch(
      /traded \$12\.5.*floor \$1(\.0)?K.*2 active price buckets \(floor 6\)/,
    );
    shows(/Over the last 24 hours it traded/);
    // A thin pair's tip stream is withheld; never open it.
    expect(useTipStream).toHaveBeenLastCalledWith(null);
  });

  it('a thin-market poll does not displace a declared-peg price', async () => {
    stubFetch(200, { data: { price: '0.42' }, flags: { thin_market: true } });
    renderPrice({
      assetID: AUDD,
      initialPrice: '0.655',
      initialProvenance: 'declared_peg',
    });
    await fetchSettled();
    await screen.findByText(/\$0\.655/);
    shows(PEG_CAPTION);
    hides(/\$0\.42/, '⚠');
  });
});

// The 24h pill must follow the live change-summary feed, not the
// build-time figure, or its arrow can contradict the live price.
describe('LiveAssetPrice — 24h change pill', () => {
  it('overrides a stale baked UP pill with the live DOWN figure', async () => {
    const change = {
      entity_type: 'coin',
      entity_id: 'native',
      h24_delta_pct: -5.2,
    };
    stubFetch(
      200,
      {
        data: {
          ...change,
          refreshed_at: '2026-09-19T15:00:00Z',
          current_value: '0.170',
        },
      },
      '/v1/changes/',
    );
    renderPrice({ initialChangePct: 2.1 });
    expect(await screen.findByText(/-5\.20%/)).toBeInTheDocument();
    hides(/\+2\.10%/);
  });

  it('keeps the baked pill when the worker has no row yet', () => {
    renderPrice({ initialChangePct: 2.1 });
    shows(/\+2\.10%/);
  });
});
