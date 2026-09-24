import { describe, expect, it } from 'vitest';
import type { BarPrice } from 'lightweight-charts';

import {
  MAX_FIXED_DECIMALS,
  priceFormatFor,
  pricePrecisionFor,
  type CandlePoint,
} from './CandleChart';

function flat(price: number): CandlePoint[] {
  return [
    { time: 1, open: price, high: price, low: price, close: price },
    { time: 2, open: price, high: price, low: price, close: price },
  ];
}

// render mirrors what the series shows for a price under priceFormatFor:
// the built-in 'price' formatter rounds to `precision` decimals.
function render(points: CandlePoint[], price: number): string {
  const fmt = priceFormatFor(points);
  if (fmt.type === 'custom') return fmt.formatter(price as BarPrice);
  return price.toFixed(fmt.precision);
}

describe('pricePrecisionFor', () => {
  it('keeps the existing tiers for ordinary magnitudes', () => {
    expect(pricePrecisionFor([])).toBe(2);
    expect(pricePrecisionFor(flat(65000))).toBe(2);
    expect(pricePrecisionFor(flat(42))).toBe(4);
    expect(pricePrecisionFor(flat(0.17))).toBe(6);
    expect(pricePrecisionFor(flat(0.0005))).toBe(8);
    expect(pricePrecisionFor(flat(0.000005))).toBe(10);
  });

  it('gives a sub-1e-10 price enough decimals to show it', () => {
    const price = 3e-11;
    const precision = pricePrecisionFor(flat(price));
    expect(price.toFixed(precision)).toBe('0.0000000000300000');
  });

  it('never exceeds the 16 fractional digits the chart library accepts', () => {
    for (let exp = 6; exp >= -24; exp--) {
      expect(pricePrecisionFor(flat(3 * 10 ** exp))).toBeLessThanOrEqual(
        MAX_FIXED_DECIMALS,
      );
    }
  });
});

describe('priceFormatFor', () => {
  it('renders every non-zero magnitude to four significant digits', () => {
    for (let exp = 6; exp >= -24; exp--) {
      const price = 1.2345 * 10 ** exp;
      const shown = Number(render(flat(price), price));
      expect(Math.abs(shown - price) / price).toBeLessThan(1e-3);
    }
  });

  it('switches to scientific notation below the fixed-decimal floor', () => {
    const fmt = priceFormatFor(flat(3e-15));
    expect(fmt.type).toBe('custom');
    expect(render(flat(3e-15), 3e-15)).toBe('3.0000e-15');
  });

  it('keeps fixed decimals for ordinary prices', () => {
    expect(priceFormatFor(flat(0.17))).toEqual({
      type: 'price',
      precision: 6,
      minMove: 1e-6,
    });
  });
});
