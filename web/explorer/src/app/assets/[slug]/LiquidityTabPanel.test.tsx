import { describe, it, expect, vi, afterEach, beforeEach } from 'vitest';
import { render, screen } from '@testing-library/react';

const ASSET = 'USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN';

function mockFetch(impl: () => Promise<Response>) {
  const fn = vi.fn(impl);
  vi.stubGlobal('fetch', fn);
  return fn;
}

function jsonResponse(body: unknown, status = 200, cacheControl?: string) {
  return new Response(JSON.stringify(body), {
    status,
    headers: {
      'content-type': 'application/json',
      ...(cacheControl ? { 'cache-control': cacheControl } : {}),
    },
  });
}

// The fetch goes through buildFetch's per-build memo and its waits: a fresh
// module per test is a fresh build, and fake timers run the waits.
async function renderPanel(scaleMismatch = false) {
  const { LiquidityTabPanel } = await import('./LiquidityTabPanel');
  const el = LiquidityTabPanel({ assetID: ASSET, code: 'USDC', scaleMismatch });
  await vi.runAllTimersAsync();
  render(await el);
}

// Frontend-honesty sweep: this build-time panel swallowed 5xx, 429 and
// its own 8s timeout into `[]` and then baked "No DEX pools observed
// touching USDC in the trailing 14 days" INTO THE STATIC EXPORT — the
// exact incident class src/lib/buildFetch.ts documents. Absent must read
// as unavailable; a served empty list keeps the honest empty claim.
describe('LiquidityTabPanel', () => {
  beforeEach(() => {
    vi.resetModules();
    vi.useFakeTimers();
  });
  afterEach(() => {
    vi.useRealTimers();
    vi.unstubAllGlobals();
  });

  // Casing variants and the asset_id route of one asset all render this
  // panel for the same asset id: one request per build pass, tallied.
  it('reads the pools once per pass for every route of one asset', async () => {
    const fn = mockFetch(async () => jsonResponse({ data: [] }));
    for (let i = 0; i < 4; i++) await renderPanel();
    expect(fn).toHaveBeenCalledTimes(1);
    const { buildRequestCounts } = await import('@/lib/buildFetch');
    expect(buildRequestCounts().get('/v1/pools')).toBe(1);
  });

  it('bakes the complete pool list after a degraded first answer', async () => {
    const replies = [
      jsonResponse({ data: [] }, 200, 'no-store'),
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
    ];
    const fn = mockFetch(async () => replies.shift()!);
    await renderPanel();
    expect(fn).toHaveBeenCalledTimes(2);
    expect(screen.getByText('soroswap')).toBeInTheDocument();
    expect(screen.queryByText(/Pool list incomplete/)).not.toBeInTheDocument();
  });

  it('marks a still-degraded pool list and lists it in the build summary', async () => {
    mockFetch(async () => jsonResponse({ data: [] }, 200, 'no-store'));
    await renderPanel();
    expect(screen.getByText(/Pool list incomplete:/)).toBeInTheDocument();
    expect(screen.queryByText(/No DEX pools touching/)).not.toBeInTheDocument();
    const { buildBakedDegraded } = await import('@/lib/buildFetch');
    expect([...buildBakedDegraded()].some((p) => p.includes(ASSET))).toBe(true);
  });

  it('says the pool list is unavailable when the fetch fails', async () => {
    mockFetch(async () => {
      throw new Error('ETIMEDOUT');
    });
    await renderPanel();
    expect(screen.getByText(/Pool list unavailable:/)).toBeInTheDocument();
    expect(screen.queryByText(/No DEX pools touching/)).not.toBeInTheDocument();
  });

  it('says the pool list is unavailable on a 503', async () => {
    mockFetch(async () => jsonResponse({}, 503));
    await renderPanel();
    expect(screen.getByText(/Pool list unavailable:/)).toBeInTheDocument();
    const { buildBakedDegraded } = await import('@/lib/buildFetch');
    expect([...buildBakedDegraded()].some((p) => p.includes(ASSET))).toBe(true);
  });

  // A static export bakes these figures; they must not read as current.
  it('captions the pool figures with the build time', async () => {
    vi.stubEnv('NEXT_PUBLIC_BUILD_TIME', '2026-11-20T10:30:12.345Z');
    try {
      mockFetch(async () => jsonResponse({ data: [] }));
      await renderPanel();
      expect(
        screen.getByText(
          /24h volume and trades as baked at deploy \(built 2026-11-20 10:30 UTC\)/,
        ),
      ).toBeInTheDocument();
    } finally {
      vi.unstubAllEnvs();
    }
  });

  it('keeps the genuine empty claim when the API returns no pools', async () => {
    mockFetch(async () => jsonResponse({ data: [] }));
    await renderPanel();
    expect(screen.getByText(/No DEX pools touching/)).toBeInTheDocument();
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
    expect(screen.getByText('0.420000')).toBeInTheDocument();
    expect(screen.queryByText(/Pool list unavailable/)).not.toBeInTheDocument();
    expect(screen.queryByText(/No DEX pools touching/)).not.toBeInTheDocument();
  });

  // last_price is normalised by the projection a decimals mismatch says is
  // wrong, so it is off by a power of ten exactly when the page refuses it.
  it('withholds the last price under the page scale refusal', async () => {
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
    await renderPanel(true);
    expect(screen.getByText('soroswap')).toBeInTheDocument();
    expect(screen.queryByText('0.420000')).not.toBeInTheDocument();
    expect(
      screen.getByText('withheld · decimals disagree'),
    ).toBeInTheDocument();
  });
});
