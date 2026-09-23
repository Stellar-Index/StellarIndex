// @vitest-environment node
//
// Cloudflare Pages edge function — no DOM needed. `workers-og`'s
// ImageResponse needs the CF Workers WASM loader (it errors under plain
// Node), so it's stubbed here with a lightweight fake Response — these
// tests target the request-gating logic (SEC-08/SEC-15/input-validation/
// kill-switch), not satori/resvg rendering.
import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';

// T301: capture the html the function rendered so a test can assert on the
// card's footer byline without needing the real satori/resvg pipeline.
const capturedHtml = { last: null, opts: null };

// T246: the real ImageResponse renders lazily inside its body stream, so a
// font-fetch or satori/resvg failure surfaces AFTER the 200 + public
// Cache-Control headers exist. `renderFails` reproduces that shape.
const renderMode = { renderFails: false };
const fontLoader = {
  impl: async () => new ArrayBuffer(8),
};

vi.mock('workers-og', () => ({
  loadGoogleFont: (...args) => fontLoader.impl(...args),
  // F087: mirrors workers-og@0.0.27's actual ImageResponse header
  // construction (`{"Content-Type":...,"Cache-Control":<default>,
  // ...opts.headers}`) so a caller that passes a differently-cased
  // 'cache-control' key reproduces the same doubled-header defect here
  // that it would against the real, WASM-only library.
  ImageResponse: class FakeImageResponse extends Response {
    constructor(html, opts) {
      capturedHtml.last = html;
      capturedHtml.opts = opts;
      const body = renderMode.renderFails
        ? new ReadableStream({
            start(c) {
              c.error(new Error('Could not find font URL'));
            },
          })
        : 'fake-png-bytes';
      super(body, {
        status: 200,
        headers: {
          'Content-Type': 'image/png',
          'Cache-Control': 'public, immutable, no-transform, max-age=31536000',
          ...opts?.headers,
        },
      });
    }
  },
}));

const { onRequest, liveSubline, TYPE_LABEL, resetOgGatesForTest } =
  await import('./[[path]].js');

// Isolate each test from the rate limiter/circuit breaker's module-scope
// state — without this, tests sharing the default (no `cf-connecting-ip`)
// IP bucket would trip the K067 rate limit depending on run order.
beforeEach(() => {
  resetOgGatesForTest();
  renderMode.renderFails = false;
  fontLoader.impl = async () => new ArrayBuffer(8);
  capturedHtml.opts = null;
});

function makeContext(pathname, env = {}, headers = {}) {
  return {
    request: new Request(`https://stellarindex.io${pathname}`, { headers }),
    env,
  };
}

describe('og function — kill-switch', () => {
  it('returns 503 and does no work when OG_DISABLED=1', async () => {
    const fetchSpy = vi.spyOn(globalThis, 'fetch');
    const res = await onRequest(
      makeContext('/og/markets/native~usdc', { OG_DISABLED: '1' }),
    );
    expect(res.status).toBe(503);
    expect(fetchSpy).not.toHaveBeenCalled();
    fetchSpy.mockRestore();
  });
});

describe('og function — type allowlist (SEC-08 / SEC-15)', () => {
  it('404s a type not in TYPE_LABEL', async () => {
    const res = await onRequest(makeContext('/og/bogus-type/whatever'));
    expect(res.status).toBe(404);
  });

  it('404s a prototype-chain key instead of rendering an inherited value', async () => {
    // Sanitization keeps only [a-z0-9-], so 'constructor'/'tostring' survive
    // untouched — a plain-object TYPE_LABEL would resolve these to
    // Object.prototype members instead of missing. Confirm the allowlist
    // truly has no own entry for them, then confirm the route 404s.
    expect(TYPE_LABEL.has('constructor')).toBe(false);
    expect(TYPE_LABEL.has('tostring')).toBe(false);
    const res = await onRequest(makeContext('/og/constructor/x'));
    expect(res.status).toBe(404);
  });

  it('still renders the id-less home fallback (not rejected by the gate)', async () => {
    const res = await onRequest(makeContext('/og/'));
    expect(res.status).toBe(200);
  });

  it('still renders a known type', async () => {
    const res = await onRequest(makeContext('/og/assets/usdc'));
    expect(res.status).toBe(200);
  });
});

describe('og function — cache-control header (F087)', () => {
  it('emits exactly the 60s edge policy, not doubled with the library default', async () => {
    const res = await onRequest(makeContext('/og/assets/usdc'));
    expect(res.headers.get('cache-control')).toBe(
      'public, s-maxage=60, stale-while-revalidate=300',
    );
  });
});

describe('og function — id length bound (input-validation)', () => {
  it('404s an oversized id before decoding/fetching/rendering', async () => {
    const fetchSpy = vi.spyOn(globalThis, 'fetch');
    const hugeId = 'a'.repeat(500);
    const res = await onRequest(makeContext(`/og/assets/${hugeId}`));
    expect(res.status).toBe(404);
    expect(fetchSpy).not.toHaveBeenCalled();
    fetchSpy.mockRestore();
  });
});

describe('og function — 404 responses are cacheable (F101)', () => {
  it('sets a public cache-control header on an unknown-type 404', async () => {
    const res = await onRequest(makeContext('/og/bogus-type/whatever'));
    expect(res.status).toBe(404);
    expect(res.headers.get('cache-control')).toMatch(/public/);
  });

  it('sets a public cache-control header on an oversized-id 404', async () => {
    const hugeId = 'a'.repeat(500);
    const res = await onRequest(makeContext(`/og/assets/${hugeId}`));
    expect(res.status).toBe(404);
    expect(res.headers.get('cache-control')).toMatch(/public/);
  });
});

describe('og function — per-IP rate limit (K067)', () => {
  it('429s a single IP once it exceeds the per-window budget', async () => {
    const headers = { 'cf-connecting-ip': '203.0.113.9' };
    let lastRes;
    for (let i = 0; i < 21; i += 1) {
      lastRes = await onRequest(makeContext('/og/assets/usdc', {}, headers));
    }
    expect(lastRes.status).toBe(429);
  });

  it('does not rate-limit a different IP sharing the same window', async () => {
    const flooded = { 'cf-connecting-ip': '203.0.113.9' };
    const other = { 'cf-connecting-ip': '203.0.113.10' };
    for (let i = 0; i < 21; i += 1) {
      await onRequest(makeContext('/og/assets/usdc', {}, flooded));
    }
    const res = await onRequest(makeContext('/og/assets/usdc', {}, other));
    expect(res.status).toBe(200);
  });
});

describe('liveSubline — upstream circuit breaker (F101)', () => {
  afterEach(() => vi.restoreAllMocks());

  it('stops calling a repeatedly-failing upstream after the failure threshold', async () => {
    const fetchSpy = vi
      .spyOn(globalThis, 'fetch')
      .mockResolvedValue(new Response('boom', { status: 500 }));
    const id =
      'native~USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN';

    // Drive the breaker past its threshold with real upstream failures.
    for (let i = 0; i < 5; i += 1) {
      await liveSubline('markets', id);
    }
    expect(fetchSpy).toHaveBeenCalledTimes(5);

    // The next call should short-circuit: no further network call made.
    const result = await liveSubline('markets', id);
    expect(result).toBeNull();
    expect(fetchSpy).toHaveBeenCalledTimes(5);
  });
});

describe('liveSubline — asset-shape guard (SEC-15)', () => {
  afterEach(() => vi.restoreAllMocks());

  it('does not call fetch when a leg contains markup/garbage', async () => {
    const fetchSpy = vi.spyOn(globalThis, 'fetch');
    const result = await liveSubline(
      'markets',
      '<img src=x>~native',
      'https://api.stellarindex.io',
    );
    expect(result).toBeNull();
    expect(fetchSpy).not.toHaveBeenCalled();
  });

  it('still calls fetch for a real canonical CODE-ISSUER pair', async () => {
    const fetchSpy = vi.spyOn(globalThis, 'fetch').mockResolvedValue(
      new Response(JSON.stringify({ data: { price: '0.12345' } }), {
        status: 200,
      }),
    );
    const result = await liveSubline(
      'markets',
      'native~USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN',
      'https://api.stellarindex.io',
    );
    expect(fetchSpy).toHaveBeenCalledTimes(1);
    expect(result).toBe('1 XLM = 0.1235 USDC');
  });
});

describe('og function — per-network API origin (network-hardcodes T283)', () => {
  afterEach(() => vi.restoreAllMocks());

  // Each network (mainnet/testnet/futurenet) is a SEPARATE Pages project on
  // its own hostname running this same function; the upstream price fetch
  // must follow the request's hostname (api.{hostname}) rather than always
  // hitting the mainnet API — a testnet card hardcoded to the mainnet API
  // would fetch mainnet prices (or 404, since testnet runs no aggregator).
  it('fetches the price from the REQUESTED network origin, not mainnet', async () => {
    const fetchSpy = vi.spyOn(globalThis, 'fetch').mockResolvedValue(
      new Response(JSON.stringify({ data: { price: '1.5' } }), {
        status: 200,
      }),
    );
    const res = await onRequest({
      request: new Request(
        'https://testnet.stellarindex.io/og/markets/native~usdc',
      ),
      env: {},
    });
    expect(res.status).toBe(200);
    expect(fetchSpy).toHaveBeenCalledTimes(1);
    const requestedUrl = fetchSpy.mock.calls[0][0];
    expect(String(requestedUrl)).toMatch(
      /^https:\/\/api\.testnet\.stellarindex\.io\/v1\/price\?/,
    );
  });

  it('renders the request host as the card byline, not the mainnet domain (T301)', async () => {
    vi.spyOn(globalThis, 'fetch').mockResolvedValue(
      new Response(JSON.stringify({ data: { price: '1.5' } }), {
        status: 200,
      }),
    );
    await onRequest({
      request: new Request('https://testnet.stellarindex.io/og/assets/usdc'),
      env: {},
    });
    expect(capturedHtml.last).toContain('testnet.stellarindex.io');
    expect(capturedHtml.last).not.toContain('>stellarindex.io<');
  });
});

describe('og function — render failure path (T246)', () => {
  afterEach(() => vi.useRealTimers());

  it('passes a preloaded font so the library never does its own unbounded font fetch', async () => {
    const res = await onRequest(makeContext('/og/assets/usdc'));
    expect(res.status).toBe(200);
    expect(capturedHtml.opts.fonts).toHaveLength(1);
    expect(capturedHtml.opts.fonts[0].data.byteLength).toBe(8);
  });

  it('serves the static card, uncached, when rendering fails mid-stream', async () => {
    renderMode.renderFails = true;
    const res = await onRequest(makeContext('/og/assets/usdc'));
    expect(res.status).toBe(302);
    expect(res.headers.get('location')).toBe('https://stellarindex.io/og.png');
    expect(res.headers.get('cache-control')).toBe('no-store');
  });

  it('serves the static card, uncached, when the font fetch rejects', async () => {
    fontLoader.impl = async () => {
      throw new Error('fonts.googleapis.com unreachable');
    };
    const res = await onRequest(makeContext('/og/assets/usdc'));
    expect(res.status).toBe(302);
    expect(res.headers.get('location')).toBe('https://stellarindex.io/og.png');
    expect(res.headers.get('cache-control')).toBe('no-store');
  });

  it('bounds a hung font fetch instead of holding the request open', async () => {
    vi.useFakeTimers();
    fontLoader.impl = () => new Promise(() => {});
    const pending = onRequest(makeContext('/og/assets/usdc'));
    await vi.advanceTimersByTimeAsync(3000);
    const res = await pending;
    expect(res.status).toBe(302);
    expect(res.headers.get('location')).toBe('https://stellarindex.io/og.png');
  });

  it('retries the font after a failure instead of pinning it for the isolate', async () => {
    fontLoader.impl = async () => {
      throw new Error('transient');
    };
    expect((await onRequest(makeContext('/og/assets/usdc'))).status).toBe(302);
    fontLoader.impl = async () => new ArrayBuffer(8);
    const res = await onRequest(makeContext('/og/assets/usdc'));
    expect(res.status).toBe(200);
    expect(await res.text()).toBe('fake-png-bytes');
  });
});
