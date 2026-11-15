import type { PriceFormat } from 'lightweight-charts';

import { formatSubunitPrice } from '@/lib/format';

// lightweight-charts' PriceFormatter throws on more than 16 fractional digits.
export const MAX_FIXED_DECIMALS = 16;
// Below this, 16 fixed decimals keep fewer than 5 significant digits
// (and print 0 under 1e-16), so the format switches to a custom formatter.
const MIN_FIXED_PRICE = 1e-12;

/** precisionForMax — axis decimals for a price series whose largest value is max. */
export function precisionForMax(max: number): number {
  if (max === 0) return 2;
  if (max >= 1000) return 2;
  if (max >= 10) return 4;
  if (max >= 0.01) return 6;
  if (max >= 0.0001) return 8;
  if (max >= 1e-6) return 10;
  if (max >= 1e-8) return 12;
  if (max >= 1e-10) return 14;
  return MAX_FIXED_DECIMALS;
}

/** priceFormatForMax — the series priceFormat for that magnitude. */
export function priceFormatForMax(max: number): PriceFormat {
  if (max === 0 || max >= MIN_FIXED_PRICE) {
    const precision = precisionForMax(max);
    return { type: 'price', precision, minMove: 10 ** -precision };
  }
  const decimals = Math.ceil(-Math.log10(max)) + 4;
  return {
    type: 'custom',
    formatter: (price: number) => formatSubunitPrice(price),
    minMove: 10 ** -decimals,
    base: 10 ** decimals,
  };
}

/**
 * priceFormatForValues — the price format for a line of prices or rates.
 * A price line with no format gets the library's 2 decimals, and a
 * sub-cent rate then reads 0.00.
 */
export function priceFormatForValues(
  values: readonly (number | null | undefined)[],
): PriceFormat {
  let max = 0;
  for (const v of values) {
    if (v != null && Number.isFinite(v) && Math.abs(v) > max) max = Math.abs(v);
  }
  return priceFormatForMax(max);
}
