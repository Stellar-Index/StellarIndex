import { beforeEach, describe, it, expect, vi } from 'vitest';
import { act, render, screen, fireEvent, within } from '@testing-library/react';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';

vi.mock('@/api/client', async () => {
  const actual =
    await vi.importActual<typeof import('@/api/client')>('@/api/client');
  return { ...actual, apiGet: vi.fn() };
});
// jsdom has no EventSource; the headline's tip stream stays closed.
vi.mock('@/lib/live/hooks', async (importOriginal) => ({
  ...(await importOriginal<typeof import('@/lib/live/hooks')>()),
  useTipStream: () => null,
}));

import { apiGet } from '@/api/client';
import {
  AssetSwap,
  assetIssuer,
  rankSwapTokens,
  type SwapToken,
} from './AssetSwap';
import { LiveAssetPrice } from './LiveAssetPrice';

// The /v1/price poll uses raw fetch; unstubbed it reached the live API and
// a real price replaced the baked one mid-test.
beforeEach(() => {
  vi.stubGlobal('fetch', vi.fn().mockRejectedValue(new Error('offline')));
});

// The converter's fiat legs come from /v1/price/batch, which
// this widget typed as `{data: Array<{asset_id, price}>}` — so
// `price_type` and `observed_at` were dropped and every fiat leg looked
// like a fresh observed quote. It is not a theoretical gap: measured on
// the live API, fiat:EUR/fiat:USD answered `observed_at:
// 2026-09-18T00:00:00Z` (~36h old) with `flags.stale: true`, and the
// widget's whole output is a money amount computed from it.

function hoursAgo(h: number): string {
  return new Date(Date.now() - h * 3_600_000).toISOString();
}

function stubApi() {
  vi.mocked(apiGet).mockImplementation(async (path: string) => {
    if (path.startsWith('/v1/price/batch')) {
      return {
        data: [
          {
            asset_id: 'fiat:EUR',
            quote: 'fiat:USD',
            price: '1.148765077541643',
            price_type: 'vwap',
            observed_at: hoursAgo(36),
          },
        ],
        as_of: new Date().toISOString(),
        sources: ['massive'],
        flags: { stale: true },
      };
    }
    // The asset catalogue behind the picker's crypto rows: empty, so the
    // list under test is exactly the forex batch's.
    return { data: [], pagination: { next: '' } };
  });
}

function renderSwap() {
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  });
  return render(
    <QueryClientProvider client={client}>
      <AssetSwap symbol="XLM" assetId="native" priceUSD="0.195" />
    </QueryClientProvider>,
  );
}

/** Opens the "receive" leg's picker and chooses EUR. */
async function pickEURAsReceiveLeg() {
  const usdButton = await screen.findByRole('button', { name: /USD/ });
  fireEvent.click(usdButton);
  const eur = await screen.findByRole('button', { name: /EUR/ });
  fireEvent.click(eur);
}

// useFiatTokens ran its /v1/price/batch fetch (and 5-minute poll)
// unconditionally on mount, before the picker that is the only place fiat
// rates are shown was ever opened.
describe('AssetSwap fiat batch fetch', () => {
  it('does not fetch fiat rates until the token picker is opened', async () => {
    stubApi();
    renderSwap();

    // Let any unconditional effect/query fire before we check.
    await screen.findByRole('button', { name: /USD/ });

    const fiatCalls = vi
      .mocked(apiGet)
      .mock.calls.filter(([path]) => path.startsWith('/v1/price/batch'));
    expect(fiatCalls).toHaveLength(0);

    await pickEURAsReceiveLeg();

    const fiatCallsAfterOpen = vi
      .mocked(apiGet)
      .mock.calls.filter(([path]) => path.startsWith('/v1/price/batch'));
    expect(fiatCallsAfterOpen.length).toBeGreaterThan(0);
  });
});

describe('AssetSwap fiat leg basis', () => {
  it('says when the fiat leg was observed instead of implying it is current', async () => {
    stubApi();
    renderSwap();
    await pickEURAsReceiveLeg();

    expect(
      await screen.findByText(/EUR price: observed 2d ago/),
    ).toBeInTheDocument();
  });

  it('still converts through the leg it is now describing', async () => {
    stubApi();
    renderSwap();
    await pickEURAsReceiveLeg();

    // 1 XLM × 0.195 USD ÷ 1.148765077541643 USD/EUR = 0.169745… EUR
    expect(await screen.findByDisplayValue(/^0\.1697/)).toBeInTheDocument();
  });

  // Once picked, the leg must not freeze at its pick-time snapshot,
  // however long the tab stays open. It must poll its own price live, the same as the page asset.
  it("re-fetches the picked leg's own price on a live interval instead of freezing at pick time", async () => {
    stubApi();
    renderSwap();
    await pickEURAsReceiveLeg();

    const legCallCount = () =>
      vi
        .mocked(apiGet)
        .mock.calls.filter(
          ([path, params]) =>
            path === '/v1/price/batch' &&
            (params as { asset_ids?: string } | undefined)?.asset_ids ===
              'fiat:EUR',
        ).length;

    await vi.waitFor(() => expect(legCallCount()).toBeGreaterThan(0));
    const before = legCallCount();

    vi.useFakeTimers();
    try {
      await vi.advanceTimersByTimeAsync(60_000);
    } finally {
      vi.useRealTimers();
    }

    await vi.waitFor(() => expect(legCallCount()).toBeGreaterThan(before));
  });
});

// The page asset's leg must follow the live /v1/price poll the headline
// reads, not the build-time baked price, and blank when the server withholds.
describe('AssetSwap page-asset leg', () => {
  function stubPrice(status: number, body: unknown) {
    vi.stubGlobal(
      'fetch',
      vi.fn().mockImplementation(async (input: string | URL) => {
        const hit = String(input).includes('/v1/price?');
        const code = hit ? status : 500;
        return { status: code, ok: code === 200, json: async () => body };
      }),
    );
  }
  const receiveValue = () =>
    (screen.getAllByRole('textbox')[1] as HTMLInputElement).value;

  it('converts at the live price, not the baked one', async () => {
    stubApi();
    stubPrice(200, { data: { price: '0.26' } });
    renderSwap();
    await vi.waitFor(() => expect(receiveValue()).toBe('0.26'));
    vi.unstubAllGlobals();
  });

  it('stops converting once the server withholds the price', async () => {
    stubApi();
    stubPrice(404, {
      type: 'https://stellarindex.io/problems/price-withheld',
      title: 'withheld',
    });
    renderSwap();
    // A dash with the reason, never a blank or a zero that reads as a value.
    await vi.waitFor(() => expect(receiveValue()).toBe('—'));
    expect(
      screen.getByText(
        'XLM has no USD price: withheld by the server (withheld)',
      ),
    ).toBeInTheDocument();
    vi.unstubAllGlobals();
  });

  // 123456789012.3456789 USD at 0.12345678901234567891 USD/XLM is
  // 999999999999.9999999 XLM exactly; a float division prints 1,000,000,000,000.
  it('converts an amount above 2^53 base units exactly', async () => {
    stubApi();
    stubPrice(200, { data: { price: '0.12345678901234567891' } });
    renderSwap();
    await vi.waitFor(() => expect(receiveValue()).toBe('0.1235'));
    fireEvent.change(screen.getAllByRole('textbox')[1], {
      target: { value: '123456789012.3456789' },
    });
    expect((screen.getAllByRole('textbox')[0] as HTMLInputElement).value).toBe(
      '999,999,999,999.9999999',
    );
    vi.unstubAllGlobals();
  });

  it('carries the thin-market warning when the live price is thin', async () => {
    stubApi();
    stubPrice(200, { data: { price: '0.26' }, flags: { thin_market: true } });
    renderSwap();
    await vi.waitFor(() => expect(receiveValue()).toBe('0.26'));
    expect(
      screen.getByText(/thin market · low confidence · in no total/),
    ).toBeInTheDocument();
    expect(screen.getByText(/^Low confidence:/)).toBeInTheDocument();
    vi.unstubAllGlobals();
  });

  // A declared peg is not the market /v1/price refuses: the converter keeps
  // the hook's derived price, re-read from /v1/assets/{id}.
  function renderPegged() {
    const base = vi.mocked(apiGet).getMockImplementation()!;
    vi.mocked(apiGet).mockImplementation(async (path: string) => {
      if (path === '/v1/assets/native')
        return { data: { price_usd: '1.08', price_basis: 'declared_peg' } };
      return base(path);
    });
    const client = new QueryClient({
      defaultOptions: { queries: { retry: false } },
    });
    return render(
      <QueryClientProvider client={client}>
        <AssetSwap
          symbol="XLM"
          assetId="native"
          priceUSD="1.07"
          priceProvenance="declared_peg"
        />
      </QueryClientProvider>,
    );
  }

  it('keeps converting at a declared peg the market poll withholds', async () => {
    stubApi();
    stubPrice(404, {
      type: 'https://stellarindex.io/problems/price-withheld',
    });
    renderPegged();
    await vi.waitFor(() => expect(receiveValue()).toBe('1.08'));
    vi.unstubAllGlobals();
  });

  it('never lets a thin market price displace a declared peg', async () => {
    stubApi();
    stubPrice(200, { data: { price: '0.5' }, flags: { thin_market: true } });
    renderPegged();
    await vi.waitFor(() => expect(receiveValue()).toBe('1.08'));
    expect(screen.queryByText(/^Low confidence:/)).not.toBeInTheDocument();
    vi.unstubAllGlobals();
  });

  // The page leg is the headline's number; it must carry what the headline
  // says about it rather than look like a fresh market quote.
  const note = () => screen.getByText(/^XLM price:/).textContent ?? '';

  it('names a declared peg as not a market price', async () => {
    stubApi();
    stubPrice(404, {
      type: 'https://stellarindex.io/problems/price-withheld',
    });
    renderPegged();
    await vi.waitFor(() => expect(receiveValue()).toBe('1.08'));
    expect(note()).toMatch(/declared 1:1 fiat peg .* not a market price/);
    vi.unstubAllGlobals();
  });

  it('marks a stale live price as stale', async () => {
    stubApi();
    stubPrice(200, { data: { price: '0.26' }, flags: { stale: true } });
    renderSwap();
    await vi.waitFor(() => expect(receiveValue()).toBe('0.26'));
    expect(note()).toMatch(/stale/);
    vi.unstubAllGlobals();
  });

  it('labels the build-time price when the poll has failed', async () => {
    stubApi();
    renderSwap();
    await vi.waitFor(() => expect(receiveValue()).toBe('0.195'));
    await vi.waitFor(() => expect(note()).toMatch(/as baked at deploy/));
  });
});

// Two independent polls could disagree: the headline withheld while the
// converter's own request failed and kept the withdrawn price.
describe('AssetSwap and the headline share one price resolution', () => {
  const receiveValue = () =>
    (screen.getAllByRole('textbox')[1] as HTMLInputElement).value;

  it('serves both from one request and empties the converter with the headline', async () => {
    stubApi();
    let withheld = false;
    const fetchMock = vi.fn().mockImplementation(async (input: string) => {
      if (!String(input).includes('/v1/price?'))
        return { status: 500, ok: false, json: async () => ({}) };
      return withheld
        ? {
            status: 404,
            ok: false,
            json: async () => ({
              type: 'https://stellarindex.io/problems/price-withheld',
              detail: 'issuer flagged',
            }),
          }
        : {
            status: 200,
            ok: true,
            json: async () => ({ data: { price: '0.26' } }),
          };
    });
    vi.stubGlobal('fetch', fetchMock);
    const priceCalls = () =>
      fetchMock.mock.calls.filter(([u]) => String(u).includes('/v1/price?'))
        .length;
    const client = new QueryClient({
      defaultOptions: { queries: { retry: false } },
    });
    render(
      <QueryClientProvider client={client}>
        <LiveAssetPrice
          assetID="native"
          initialPrice="0.195"
          initialProvenance="vwap1m"
        />
        <AssetSwap
          symbol="XLM"
          assetId="native"
          priceUSD="0.195"
          priceProvenance="vwap1m"
        />
      </QueryClientProvider>,
    );
    await vi.waitFor(() => expect(receiveValue()).toBe('0.26'));
    expect(screen.getByText(/^\$0\.26/)).toBeInTheDocument();
    expect(priceCalls()).toBe(1);

    withheld = true;
    await act(() => client.refetchQueries({ queryKey: ['/v1/price'] }));
    expect(priceCalls()).toBe(2);
    // Checked in the render that first shows the verdict, not eventually.
    await vi.waitFor(() =>
      expect(screen.getByText('issuer flagged')).toBeInTheDocument(),
    );
    expect(receiveValue()).toBe('—');
    vi.unstubAllGlobals();
  });
});

// Production serves a catalogue row with asset_id = its slug, which
// /v1/price/batch rejects with a 400; the leg must price the issuance.
describe('AssetSwap catalogue leg', () => {
  const USDC = 'USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN';

  it('prices a picked catalogue row through its verified issuance id', async () => {
    stubApi();
    const base = vi.mocked(apiGet).getMockImplementation()!;
    vi.mocked(apiGet).mockImplementation(async (path: string) => {
      if (path === '/v1/assets/verified')
        return { data: [{ slug: 'usdc', asset_id: USDC }] };
      if (path.startsWith('/v1/assets'))
        return {
          data: [
            {
              asset_id: 'usdc',
              slug: 'usdc',
              code: 'USDC',
              type: 'global',
              price_usd: '1.0001',
            },
          ],
          pagination: { next: '' },
        };
      return base(path);
    });
    renderSwap();
    fireEvent.click(await screen.findByRole('button', { name: /USD/ }));
    fireEvent.click(await screen.findByRole('button', { name: /USDC/ }));

    const legIds = () =>
      vi
        .mocked(apiGet)
        .mock.calls.filter(([p]) => p === '/v1/price/batch')
        .map(([, params]) => (params as { asset_ids?: string }).asset_ids);
    await vi.waitFor(() => expect(legIds()).toContain(USDC));
    expect(legIds()).not.toContain('usdc');
  });

  it('keeps a crypto row with no issuance here and prices it as crypto:<TICKER>', async () => {
    stubApi();
    const base = vi.mocked(apiGet).getMockImplementation()!;
    vi.mocked(apiGet).mockImplementation(async (path: string) => {
      if (path === '/v1/assets/verified') return { data: [{ slug: 'btc' }] };
      if (path.startsWith('/v1/assets'))
        return {
          data: [
            {
              asset_id: 'btc',
              slug: 'btc',
              code: 'BTC',
              class: 'crypto',
              type: 'global',
              price_usd: '97000',
            },
          ],
          pagination: { next: '' },
        };
      return base(path);
    });
    renderSwap();
    fireEvent.click(await screen.findByRole('button', { name: /USD/ }));
    fireEvent.click(await screen.findByRole('button', { name: /BTC/ }));

    const legIds = () =>
      vi
        .mocked(apiGet)
        .mock.calls.filter(([p]) => p === '/v1/price/batch')
        .map(([, params]) => (params as { asset_ids?: string }).asset_ids);
    await vi.waitFor(() => expect(legIds()).toContain('crypto:BTC'));
    expect(legIds()).not.toContain('btc');
  });

  it("never lists the page asset's catalogue row once its price is withheld", async () => {
    stubApi();
    const base = vi.mocked(apiGet).getMockImplementation()!;
    vi.mocked(apiGet).mockImplementation(async (path: string) => {
      if (path.startsWith('/v1/assets'))
        return {
          data: [{ asset_id: 'native', code: 'XLM', price_usd: '0.19' }],
          pagination: { next: '' },
        };
      return base(path);
    });
    vi.stubGlobal(
      'fetch',
      vi.fn().mockImplementation(async () => ({
        status: 404,
        ok: false,
        json: async () => ({
          type: 'https://stellarindex.io/problems/price-withheld',
        }),
      })),
    );
    renderSwap();
    await vi.waitFor(() =>
      expect(
        (screen.getAllByRole('textbox')[1] as HTMLInputElement).value,
      ).toBe('—'),
    );
    fireEvent.click(await screen.findByRole('button', { name: /USD/ }));
    const dialog = await screen.findByRole('dialog');
    await within(dialog).findByRole('button', { name: /EUR/ });
    expect(
      within(dialog).queryByRole('button', { name: /XLM/ }),
    ).not.toBeInTheDocument();
    vi.unstubAllGlobals();
  });
});

// TokenPicker hand-rolled an Escape listener instead of the shared
// useDialog contract: no dialog role, no focus trap, and focus fell to
// <body> on close instead of returning to the leg the user opened it from.
describe('AssetSwap token picker dialog contract', () => {
  it('is announced as a modal dialog', async () => {
    stubApi();
    renderSwap();
    fireEvent.click(await screen.findByRole('button', { name: /USD/ }));

    const dialog = await screen.findByRole('dialog');
    expect(dialog).toHaveAttribute('aria-modal', 'true');
    expect(dialog).toHaveAccessibleName();
  });

  it('returns focus to the leg trigger when closed with Escape', async () => {
    stubApi();
    renderSwap();
    const trigger = await screen.findByRole('button', { name: /USD/ });
    trigger.focus();
    fireEvent.click(trigger);
    await screen.findByRole('button', { name: /EUR/ });

    fireEvent.keyDown(document, { key: 'Escape' });

    expect(
      screen.queryByPlaceholderText(/Search assets/),
    ).not.toBeInTheDocument();
    expect(document.activeElement).toBe(trigger);
  });

  it('returns focus to the leg trigger after a token is picked', async () => {
    stubApi();
    renderSwap();
    const trigger = await screen.findByRole('button', { name: /USD/ });
    trigger.focus();
    fireEvent.click(trigger);
    fireEvent.click(await screen.findByRole('button', { name: /EUR/ }));

    expect(
      screen.queryByPlaceholderText(/Search assets/),
    ).not.toBeInTheDocument();
    expect(document.activeElement).toBe(
      await screen.findByRole('button', { name: /EUR/ }),
    );
  });

  it('keeps Tab inside the picker', async () => {
    stubApi();
    renderSwap();
    fireEvent.click(await screen.findByRole('button', { name: /USD/ }));
    const search = await screen.findByPlaceholderText(/Search assets/);

    // The trap claims the key (preventDefault) so the browser's default
    // move to the next page control never happens.
    const notCancelled = fireEvent.keyDown(search, { key: 'Tab' });
    expect(notCancelled).toBe(false);
  });
});

// A catalogue row priced by a peg or a two-hop route is not a market quote;
// picking it must carry that basis (and when it was priced) into the note.
describe('AssetSwap catalogue leg basis', () => {
  const AUDD = 'AUDD-GDC7X2MXTYSAKUUGAIQ7J7RPEIM7GXSAIWFYWWH4GLNFECQVJJLB2EEU';

  it.each([
    [
      'declared_peg',
      /AUDD price: declared 1:1 fiat peg × fx rate, not a market price · observed 3h ago/,
    ],
    ['transitive', /AUDD price: derived two-hop DEX route · observed 3h ago/],
  ] as const)('labels a picked %s row in the note', async (basis, note) => {
    stubApi();
    const base = vi.mocked(apiGet).getMockImplementation()!;
    vi.mocked(apiGet).mockImplementation(async (path: string, params) => {
      const ids = (params as { asset_ids?: string } | undefined)?.asset_ids;
      if (path === '/v1/price/batch' && ids === AUDD)
        throw new Error('offline');
      if (path.startsWith('/v1/assets'))
        return {
          data: [
            {
              asset_id: AUDD,
              code: 'AUDD',
              price_usd: '0.655',
              price_basis: basis,
              price_as_of: hoursAgo(3),
            },
          ],
          pagination: { next: '' },
        };
      return base(path);
    });
    renderSwap();
    fireEvent.click(await screen.findByRole('button', { name: /USD/ }));
    fireEvent.click(await screen.findByRole('button', { name: /AUDD/ }));
    await vi.waitFor(() =>
      expect(screen.getByText(/AUDD price:/).textContent).toMatch(note),
    );
  });
});

describe('AssetSwap token picker crypto rows', () => {
  it("labels a crypto row with the catalogue's asset name", async () => {
    stubApi();
    const base = vi.mocked(apiGet).getMockImplementation()!;
    vi.mocked(apiGet).mockImplementation(async (path: string) => {
      if (path.startsWith('/v1/assets')) {
        return {
          data: [
            {
              asset_id:
                'USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN',
              slug: 'usdc',
              code: 'USDC',
              issuer:
                'GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN',
              name: 'USD Coin',
              price_usd: '1.0001',
            },
          ],
          pagination: { next: '' },
        };
      }
      return base(path);
    });
    renderSwap();
    fireEvent.click(await screen.findByRole('button', { name: /USD/ }));

    const row = await screen.findByRole('button', { name: /USDC/ });
    expect(row).toHaveTextContent('USD Coin');
    expect(row).not.toHaveTextContent('Stellar asset');
  });
});

describe('swap picker look-alikes', () => {
  const G1 = 'GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN';
  const G2 = 'GDUKMGUGDZQK6YHYA5Z6AY2G4XDSZPSZ3SW5UN3ARVMO6QSRDWP5YLEX';
  const tok = (key: string, extra: Partial<SwapToken>): SwapToken => ({
    key,
    symbol: 'USDC',
    usdPrice: '1',
    kind: 'crypto',
    ...extra,
  });

  it('reads the issuer out of a canonical asset id', () => {
    expect(assetIssuer(`USDC-${G1}`)).toBe(G1);
    expect(assetIssuer('native')).toBeNull();
    expect(assetIssuer('fiat:USD')).toBeNull();
  });

  it('puts the verified asset above a same-code row and a collision last', () => {
    const fake = tok(`USDC-${G2}`, {});
    const flagged = tok('USDC-flagged', { collision: true });
    const real = tok(`USDC-${G1}`, { verified: true });
    const other = tok('native', { symbol: 'XLM' });

    expect(rankSwapTokens([flagged, fake, real], 'usdc')).toEqual([
      real,
      fake,
      flagged,
    ]);
    // No query: catalogue order stands, but a collision still sinks.
    expect(rankSwapTokens([flagged, fake, other, real], '')).toEqual([
      fake,
      other,
      real,
      flagged,
    ]);
  });

  it('shows the issuer and the verified mark on a crypto row', async () => {
    stubApi();
    const base = vi.mocked(apiGet).getMockImplementation()!;
    vi.mocked(apiGet).mockImplementation(async (path: string) => {
      if (path.startsWith('/v1/assets/verified')) {
        return { data: [{ slug: 'usdc', asset_id: `USDC-${G1}` }] };
      }
      if (path.startsWith('/v1/assets')) {
        return {
          data: [
            {
              asset_id: `USDC-${G1}`,
              slug: 'usdc',
              code: 'USDC',
              name: 'USD Coin',
              price_usd: '1.0001',
            },
            {
              asset_id: `USDC-${G2}`,
              slug: `USDC-${G2}`,
              code: 'USDC',
              name: 'USD Coin',
              price_usd: '0.5',
            },
          ],
          pagination: { next: '' },
        };
      }
      return base(path);
    });
    renderSwap();
    fireEvent.click(await screen.findByRole('button', { name: /USD/ }));

    const rows = await screen.findAllByRole('button', { name: /USDC/ });
    expect(rows).toHaveLength(2);
    expect(rows[0]).toHaveTextContent('GA5Z…KZVN');
    expect(rows[1]).toHaveTextContent('GDUK…YLEX');
    expect(
      await screen.findAllByRole('img', { name: 'Verified currency' }),
    ).toHaveLength(1);
  });

  it('never badges a classic asset whose bare code matches a fiat slug', async () => {
    stubApi();
    const base = vi.mocked(apiGet).getMockImplementation()!;
    vi.mocked(apiGet).mockImplementation(async (path: string) => {
      if (path.startsWith('/v1/assets/verified')) {
        return { data: [{ slug: 'euro', ticker: 'EUR', class: 'fiat' }] };
      }
      if (path.startsWith('/v1/assets')) {
        return {
          data: [
            {
              asset_id: `EURO-${G2}`,
              slug: 'EURO',
              code: 'EURO',
              name: 'Euro',
              price_usd: '1.08',
            },
          ],
          pagination: { next: '' },
        };
      }
      return base(path);
    });
    renderSwap();
    fireEvent.click(await screen.findByRole('button', { name: /USD/ }));

    expect(
      await screen.findByRole('button', { name: /EURO/ }),
    ).toBeInTheDocument();
    expect(
      screen.queryByRole('img', { name: 'Verified currency' }),
    ).not.toBeInTheDocument();
  });
});
