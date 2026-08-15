import { describe, it, expect } from 'vitest';
import { fiatSlugFor, assetHrefFor } from './fiat-slugs';

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
