import { describe, it, expect } from 'vitest';

import type { components } from '@/api/types';
import { coverageCaption, toChartBar } from './MarketChart';

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

describe('MarketChart coverage caption', () => {
  const DAY = 86_400;
  const start = Date.UTC(2026, 6, 1) / 1000;
  const at = (time: number) => ({ time, open: 1, high: 1, low: 1, close: 1 });

  it('states where a short series starts without claiming a backfill', () => {
    // A pair first traded 10 days into a 30-day window is young, not
    // necessarily still loading; the caption must not assert either cause.
    const data = [at(start), at(start + 10 * DAY)];
    const caption = coverageCaption(data, 30 * DAY);
    expect(caption).toBe('History from 2026-07-01.');
    expect(caption).not.toMatch(/backfill/i);
  });

  it('omits the caption when the series spans the window', () => {
    expect(
      coverageCaption([at(start), at(start + 29 * DAY)], 30 * DAY),
    ).toBeNull();
  });

  it('omits the caption for an empty series', () => {
    expect(coverageCaption([], 30 * DAY)).toBeNull();
  });
});
