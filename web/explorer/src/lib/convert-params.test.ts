import { describe, expect, it } from 'vitest';

import { buildConvertParams, convertQuery } from './convert-params';

describe('buildConvertParams', () => {
  it('never emits a hub ticker the catalogue does not serve', () => {
    // PLN is a hub but absent here: baking /convert/PLN/* fails the export
    // because the identity read 404s/400s for an unserved ticker.
    const pairs = buildConvertParams(['USD', 'EUR', 'ISK']);
    const seen = new Set(pairs.flatMap((p) => [p.from, p.to]));
    expect(seen.has('PLN')).toBe(false);
    expect(pairs).toContainEqual({ from: 'USD', to: 'ISK' });
    expect(pairs).toContainEqual({ from: 'ISK', to: 'EUR' });
    expect(pairs.some((p) => p.from === p.to)).toBe(false);
  });
});

describe('convertQuery', () => {
  it('reads fiat and XLM forward when the quote is fiat', () => {
    expect(convertQuery('EUR', 'USD')).toEqual({
      asset: 'fiat:EUR',
      quote: 'fiat:USD',
      invert: false,
    });
    expect(convertQuery('XLM', 'USD')).toEqual({
      asset: 'native',
      quote: 'fiat:USD',
      invert: false,
    });
  });

  it('reads fiat to XLM as XLM in that fiat, inverted', () => {
    expect(convertQuery('USD', 'XLM')).toEqual({
      asset: 'native',
      quote: 'fiat:USD',
      invert: true,
    });
  });
});
