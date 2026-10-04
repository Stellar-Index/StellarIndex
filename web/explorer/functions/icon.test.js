// @vitest-environment node
import { describe, it, expect, vi, afterEach } from 'vitest';
import { readFileSync } from 'node:fs';
import { isProxyableUrl, onRequest } from './icon.js';

describe('CSP img-src', () => {
  it.each(['public/_headers', 'functions/_shared/shellFallback.js'])(
    '%s keeps icons same-origin',
    (file) => {
      const directives =
        readFileSync(file, 'utf8').match(/img-src '[^;"]*/g) ?? [];
      expect(directives.length).toBeGreaterThan(0);
      for (const d of directives) expect(d).toBe("img-src 'self' data:");
    },
  );
});

const req = (u, method = 'GET') =>
  new Request(`https://stellarindex.io/icon?u=${encodeURIComponent(u)}`, {
    method,
  });

afterEach(() => vi.unstubAllGlobals());

describe('isProxyableUrl', () => {
  it('accepts public https hostnames', () => {
    expect(isProxyableUrl('https://centre.io/icon.png')).toBe(true);
  });
  it.each([
    'http://centre.io/a.png',
    'https://user:pw@centre.io/a.png',
    'https://centre.io:8443/a.png',
    'https://127.0.0.1/a.png',
    'https://2130706433/a.png',
    'https://0x7f.1/a.png',
    'https://[::1]/a.png',
    'https://localhost/a.png',
    'https://metadata.internal/a.png',
    'https://printer.local/a.png',
    'https://intranet/a.png',
    'not a url',
    '',
  ])('rejects %s', (u) => {
    expect(isProxyableUrl(u)).toBe(false);
  });
});

describe('/icon', () => {
  it('rejects an unsafe target or method without fetching', async () => {
    const f = vi.fn();
    vi.stubGlobal('fetch', f);
    const bad = await onRequest({ request: req('https://10.0.0.1/a.png') });
    expect(bad.status).toBe(400);
    const post = await onRequest({
      request: req('https://centre.io/a', 'POST'),
    });
    expect(post.status).toBe(405);
    expect(f).not.toHaveBeenCalled();
  });

  it('proxies an image with sandboxing headers', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn(
        async () =>
          new Response(new Uint8Array([1, 2, 3]), {
            headers: { 'content-type': 'image/png' },
          }),
      ),
    );
    const res = await onRequest({ request: req('https://centre.io/a.png') });
    expect(res.status).toBe(200);
    expect(res.headers.get('content-type')).toBe('image/png');
    expect(res.headers.get('content-security-policy')).toContain('sandbox');
    expect((await res.arrayBuffer()).byteLength).toBe(3);
  });

  it.each([
    ['image/x-icon', 'image/x-icon'],
    ['image/vnd.microsoft.icon', 'image/vnd.microsoft.icon'],
    ['image/bmp', 'image/bmp'],
    ['image/jpg', 'image/jpeg'],
  ])('accepts the %s alias', async (type, served) => {
    vi.stubGlobal(
      'fetch',
      vi.fn(
        async () =>
          new Response(new Uint8Array([1, 2, 3]), {
            headers: { 'content-type': type },
          }),
      ),
    );
    const res = await onRequest({ request: req('https://centre.io/a.ico') });
    expect(res.status).toBe(200);
    expect(res.headers.get('content-type')).toBe(served);
  });

  it('refuses non-image content', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn(
        async () =>
          new Response('<html>', { headers: { 'content-type': 'text/html' } }),
      ),
    );
    const res = await onRequest({ request: req('https://centre.io/a') });
    expect(res.status).toBe(415);
  });

  it('refuses an oversized body', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn(
        async () =>
          new Response(new Uint8Array(1024 * 1024 + 1), {
            headers: { 'content-type': 'image/png' },
          }),
      ),
    );
    const res = await onRequest({ request: req('https://centre.io/a.png') });
    expect(res.status).toBe(413);
  });

  it('does not follow a redirect to a private host', async () => {
    const f = vi.fn(
      async () =>
        new Response(null, {
          status: 302,
          headers: { location: 'https://169.254.169.254/x' },
        }),
    );
    vi.stubGlobal('fetch', f);
    const res = await onRequest({ request: req('https://centre.io/a.png') });
    expect(res.status).toBe(404);
    expect(f).toHaveBeenCalledTimes(1);
  });
});
