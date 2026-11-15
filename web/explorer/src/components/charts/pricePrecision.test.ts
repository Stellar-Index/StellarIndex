import { describe, expect, it } from 'vitest';

import { priceFormatForValues } from './pricePrecision';

describe('priceFormatForValues', () => {
  it('gives a sub-cent rate enough decimals to be read', () => {
    const fmt = priceFormatForValues([0.0000042, 0.0000039, null]);
    expect(fmt.type).toBe('price');
    if (fmt.type !== 'price') return;
    expect(fmt.precision).toBe(10);
    expect((0.0000042).toFixed(fmt.precision)).toBe('0.0000042000');
  });

  it('keeps two decimals at four figures and above', () => {
    expect(priceFormatForValues([98000.5, 101250])).toEqual({
      type: 'price',
      precision: 2,
      minMove: 0.01,
    });
  });

  it('ignores non-finite points and falls back to two decimals when empty', () => {
    expect(priceFormatForValues([NaN, Infinity, undefined])).toMatchObject({
      precision: 2,
    });
  });
});
