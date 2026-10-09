import { describe, it, expect } from 'vitest';

import { onRequest } from './convert/[[path]].js';
import {
  canonicalConvertTicker,
  parseConvertPath,
} from './_shared/convertSlug.js';

const BAKED = new Set(['/convert/USD/EUR/', '/convert/EUR/USD/']);

function ctx(pathname) {
  return {
    request: new Request(`https://stellarindex.io${pathname}`),
    env: {
      ASSETS: {
        fetch: async (req) =>
          BAKED.has(new URL(req.url).pathname)
            ? new Response('page', { status: 200 })
            : new Response('nf', { status: 404 }),
      },
    },
  };
}

describe('canonicalConvertTicker', () => {
  it.each([
    ['usd', 'USD'],
    ['Eur', 'EUR'],
    ['xlm', 'XLM'],
    ['native', 'XLM'],
    ['crypto:XLM', 'XLM'],
    ['fiat:gbp', 'GBP'],
    ['%20jpy%20', 'JPY'],
    ['usdt0', 'USDT0'],
    ['yxlm', 'YXLM'],
  ])('%s -> %s', (slug, want) => {
    expect(canonicalConvertTicker(slug)).toBe(want);
  });

  it.each([
    'USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN',
    'usdc:ga5z',
    'x',
    'us-dollar',
    '%E0%A4%A',
  ])('rejects %s rather than keying on a bare code', (slug) => {
    expect(canonicalConvertTicker(slug)).toBeNull();
  });

  it('rejects a same-ticker pair', () => {
    expect(parseConvertPath('/convert/xlm/native/')).toBeNull();
  });
});

describe('/convert/{from}/{to} Function', () => {
  it('301s a lower-case slug to its baked page, keeping the query', async () => {
    const res = await onRequest(ctx('/convert/usd/eur?amount=5'));
    expect(res.status).toBe(301);
    expect(res.headers.get('Location')).toBe('/convert/USD/EUR/?amount=5');
    expect(res.headers.get('X-Frame-Options')).toBe('DENY');
  });

  it('serves the canonical page itself', async () => {
    const res = await onRequest(ctx('/convert/USD/EUR/'));
    expect(res.status).toBe(200);
    expect(await res.text()).toBe('page');
  });

  it('302s a pair with no baked page to the picker', async () => {
    const res = await onRequest(ctx('/convert/xlm/usdc'));
    expect(res.status).toBe(302);
    expect(res.headers.get('Location')).toBe('/convert/?from=XLM&to=USDC');
  });

  it('passes other paths through to the static asset', async () => {
    const res = await onRequest(ctx('/convert/USD/EUR/index.txt'));
    expect(res.status).toBe(404);
  });
});
