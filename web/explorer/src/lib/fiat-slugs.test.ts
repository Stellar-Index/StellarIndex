import { readdirSync, readFileSync, statSync } from 'node:fs';
import { join } from 'node:path';

import { describe, it, expect } from 'vitest';
import { fiatSlugFor, assetHrefFor, assetHref } from './fiat-slugs';

describe('fiatSlugFor', () => {
  it('maps a known ticker to its friendly catalogue slug', () => {
    expect(fiatSlugFor('USD')).toBe('us-dollar');
    expect(fiatSlugFor('KRW')).toBe('south-korean-won');
  });

  it('is case-insensitive on the ticker', () => {
    expect(fiatSlugFor('usd')).toBe('us-dollar');
  });

  it('falls back to the lower-cased ticker for an unknown code', () => {
    expect(fiatSlugFor('AED')).toBe('aed');
  });
});

describe('assetHrefFor', () => {
  // UXP-12/AM-16: in-app fiat nav must target the DECLARED canonical
  // /external/assets/{slug}, not the non-canonical /assets/{slug}
  // VerifiedCurrencyView that generateMetadata tells crawlers to ignore.
  it('routes to the canonical /external/assets/{slug}, not /assets/{slug}', () => {
    expect(assetHrefFor('USD')).toBe('/external/assets/us-dollar');
    expect(assetHrefFor('USD')).not.toBe('/assets/us-dollar');
  });

  it('routes an unknown ticker to /external/assets/{lower-ticker}', () => {
    expect(assetHrefFor('AED')).toBe('/external/assets/aed');
  });
});

describe('assetHref', () => {
  it('routes a resolved on-chain slug to /assets/{slug}', () => {
    expect(assetHref('native')).toBe('/assets/native');
    expect(assetHref('USDC-GA5Z')).toBe('/assets/USDC-GA5Z');
  });

  it('percent-encodes the slug', () => {
    expect(assetHref('a/b')).toBe('/assets/a%2Fb');
  });
});

// GH-894/K064: a hand-built `/assets/${slug}` template literal is exactly
// how the market-pair badge's `fiat:` branch drifted past assetHrefFor and
// linked a fiat leg to the non-canonical /assets/ page. assetHref/
// assetHrefFor above are the two sanctioned owners of that URL shape;
// every other source file must call one of them rather than hand-build
// the string again.
const SRC = join(__dirname, '..');
const OWNER_FILE = 'lib/fiat-slugs.ts';
const HANDBUILT_ASSETS_HREF = /href:\s*`\/assets\/|href=\{`\/assets\//;

function sourceFiles(): Array<[string, string]> {
  const out: Array<[string, string]> = [];
  const walk = (dir: string) => {
    for (const entry of readdirSync(dir)) {
      const full = join(dir, entry);
      if (statSync(full).isDirectory()) {
        if (entry === 'node_modules' || entry === '.next') continue;
        walk(full);
        continue;
      }
      if (!/\.tsx?$/.test(entry)) continue;
      if (/\.(test|spec)\.tsx?$/.test(entry)) continue;
      out.push([full.slice(SRC.length + 1), readFileSync(full, 'utf8')]);
    }
  };
  walk(SRC);
  return out;
}

describe('asset href chokepoint guard (GH-894/K064)', () => {
  it('no source file hand-builds an /assets/ href outside the owner module', () => {
    const offenders = sourceFiles()
      .filter(([path]) => path !== OWNER_FILE)
      .filter(([, body]) => HANDBUILT_ASSETS_HREF.test(body))
      .map(([path]) => path)
      .sort();
    expect(offenders).toEqual([]);
  });

  it('the guard catches a hand-built href (does not just always pass)', () => {
    const offender = 'href={`/assets/${slug}`}';
    expect(HANDBUILT_ASSETS_HREF.test(offender)).toBe(true);
    expect(HANDBUILT_ASSETS_HREF.test('href={assetHref(slug)}')).toBe(false);
  });
});
