import fs from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';
import { describe, it, expect } from 'vitest';

import { onRequest } from './assets/[[path]].js';
import { FIAT_ASSET_SLUGS } from './_shared/fiatAssetSlugs.js';

const here = path.dirname(fileURLToPath(import.meta.url));

function ctx(pathname) {
  return {
    request: new Request(`https://stellarindex.io${pathname}`),
    env: { ASSETS: { fetch: async () => new Response('nf', { status: 404 }) } },
  };
}

describe('/assets/{fiat} bare form', () => {
  it.each([...FIAT_ASSET_SLUGS])(
    '301s /assets/%s to its external page',
    async (slug) => {
      const res = await onRequest(ctx(`/assets/${slug}`));
      expect(res.status).toBe(301);
      expect(res.headers.get('Location')).toBe(`/external/assets/${slug}/`);
      expect(res.headers.get('X-Frame-Options')).toBe('DENY');
    },
  );

  it('keeps the query string', async () => {
    const res = await onRequest(ctx('/assets/euro?tab=markets'));
    expect(res.headers.get('Location')).toBe(
      '/external/assets/euro/?tab=markets',
    );
  });

  it('does not redirect a non-fiat slug', async () => {
    const res = await onRequest(ctx('/assets/usdt-gasu4kif'));
    expect(res.status).not.toBe(301);
  });

  it('matches the /assets/{slug}/ rules in _redirects', () => {
    const text = fs.readFileSync(
      path.join(here, '../public/_redirects'),
      'utf8',
    );
    const fromRules = [
      ...text.matchAll(
        /^\/assets\/([a-z-]+)\/\s+\/external\/assets\/\1\/\s+301$/gm,
      ),
    ]
      .map((m) => m[1])
      .sort();
    expect(fromRules).toEqual([...FIAT_ASSET_SLUGS].sort());
  });

  it('matches the fiat slugs in src/lib/fiat-slugs.ts', () => {
    const src = fs.readFileSync(
      path.join(here, '../src/lib/fiat-slugs.ts'),
      'utf8',
    );
    const fromTs = [...src.matchAll(/^ {2}[A-Z]{3}: '([a-z-]+)',$/gm)]
      .map((m) => m[1])
      .sort();
    expect(fromTs).toEqual([...FIAT_ASSET_SLUGS].sort());
  });
});
