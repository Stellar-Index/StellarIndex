import { describe, it, expect } from 'vitest';

import type { components } from '@/api/types';
import { toChartBar } from './MarketChart';

type OHLCBar = components['schemas']['OHLCSeriesBar'];

// GH-1152: the candlestick volume pane plotted `Number(b.v_quote)` — a raw
// smallest-unit sum at the scale of the venues in the bucket — so every
// chart's volume was 10^6..10^8 times the market's. Each bar now states its
// own scale and the chart must divide by it, per bar.
function bar(vQuote: string, decimals?: number): OHLCBar {
  return {
    t: '2026-07-03T20:00:00Z',
    o: '0.2',
    h: '0.25',
    l: '0.19',
    c: '0.21',
    v_base: '0',
    v_quote: vQuote,
    v_base_decimals: decimals ?? null,
    v_quote_decimals: decimals ?? null,
    n: 1,
  };
}

describe('MarketChart series volume scale', () => {
  it('plots an 8dp CEX bucket and a 7dp on-chain bucket in quote units', () => {
    // 220 USD in each bucket, at two different venue scales.
    expect(toChartBar(bar('22000000000.0000000000', 8)).volume).toBe(220);
    expect(toChartBar(bar('2200000000', 7)).volume).toBe(220);
  });

  it('scales the fractional v_quote the fiat combine renders', () => {
    // The spec example: 359,971,028,467,214.0000042405 smallest units at 8dp.
    expect(toChartBar(bar('359971028467214.0000042405', 8)).volume).toBeCloseTo(
      3599710.28467214,
      6,
    );
  });

  it('plots no volume for a bar that states no scale', () => {
    expect(toChartBar(bar('2200000000')).volume).toBeUndefined();
  });
});
