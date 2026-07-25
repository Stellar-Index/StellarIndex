// @vitest-environment node
//
// Cloudflare Pages edge function — no DOM needed. `workers-og`'s
// ImageResponse needs the CF Workers WASM loader (it errors under plain
// Node), so it's stubbed here with a lightweight fake Response — these
// tests target the request-gating logic (SEC-08/SEC-15/input-validation/
// kill-switch), not satori/resvg rendering.
import { describe, it, expect, vi, afterEach } from 'vitest';

vi.mock('workers-og', () => ({
  ImageResponse: class FakeImageResponse extends Response {
    constructor(_html, opts) {
      super('fake-png-bytes', { status: 200, headers: opts?.headers });
    }
  },
}));

const { onRequest, liveSubline, TYPE_LABEL } = await import('./[[path]].js');

function makeContext(pathname, env = {}) {
  return { request: new Request(`https://stellarindex.io${pathname}`), env };
}

describe('og function — kill-switch', () => {
  it('returns 503 and does no work when OG_DISABLED=1', async () => {
    const fetchSpy = vi.spyOn(globalThis, 'fetch');
    const res = await onRequest(makeContext('/og/markets/native~usdc', { OG_DISABLED: '1' }));
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

describe('liveSubline — asset-shape guard (SEC-15)', () => {
  afterEach(() => vi.restoreAllMocks());

  it('does not call fetch when a leg contains markup/garbage', async () => {
    const fetchSpy = vi.spyOn(globalThis, 'fetch');
    const result = await liveSubline('markets', '<img src=x>~native');
    expect(result).toBeNull();
    expect(fetchSpy).not.toHaveBeenCalled();
  });

  it('still calls fetch for a real canonical CODE-ISSUER pair', async () => {
    const fetchSpy = vi
      .spyOn(globalThis, 'fetch')
      .mockResolvedValue(new Response(JSON.stringify({ data: { price: '0.12345' } }), { status: 200 }));
    const result = await liveSubline(
      'markets',
      'native~USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN',
    );
    expect(fetchSpy).toHaveBeenCalledTimes(1);
    expect(result).toBe('1 XLM = 0.1235 USDC');
  });
});
