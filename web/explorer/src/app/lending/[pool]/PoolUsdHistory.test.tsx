import { describe, it, expect } from 'vitest';

import { usdSeries, type PoolUsdPoint } from './PoolUsdHistory';

const XLM = 'CAS3J7GYLGXMF6TDJBBYYSE3HQ6BBSMLNUQ34T6TZMYMW2EVH34XOWMA';
const ODD = 'CABQAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAVX';

const point = (over: Partial<PoolUsdPoint>): PoolUsdPoint => ({
  t: '2026-11-01T00:00:00Z',
  tvl_usd: null,
  borrowed_usd: null,
  lower_bound: false,
  unvalued_reserves: [],
  price_carried_reserves: [],
  reserves: [],
  ...over,
});

const reserve = (
  asset: string,
  supplied: string | null,
  borrowed: string | null,
) => ({
  asset,
  supplied_usd: supplied,
  borrowed_usd: borrowed,
  price_usd: null,
  price_at: null,
  usd_reason: supplied == null ? ('no_price_within_bound' as const) : null,
});

describe('usdSeries', () => {
  const points = [
    point({
      tvl_usd: '10.00',
      borrowed_usd: '4.00',
      lower_bound: true,
      unvalued_reserves: [ODD],
      reserves: [reserve(XLM, '10.00', '4.00'), reserve(ODD, null, null)],
    }),
    point({
      t: '2026-11-02T00:00:00Z',
      reserves: [reserve(XLM, null, null)],
      unvalued_reserves: [XLM],
    }),
  ];

  it('leaves an unvalued bucket as a gap, never zero', () => {
    const s = usdSeries(points, []);
    expect(s.supplied.map((p) => p.value)).toEqual([10, null]);
    expect(s.borrowed.map((p) => p.value)).toEqual([4, null]);
    expect(s.supplied[1].time - s.supplied[0].time).toBe(86_400);
  });

  it('counts lower-bound buckets and names every reserve left out', () => {
    const s = usdSeries(points, ['COMITTED']);
    expect(s.lowerBound).toBe(1);
    expect(s.leftOut).toEqual(['COMITTED', ODD, XLM]);
  });

  it("charts one reserve's own values when an asset is given", () => {
    const s = usdSeries(points, ['COMITTED'], ODD);
    expect(s.supplied.map((p) => p.value)).toEqual([null, null]);
    expect(usdSeries(points, [], XLM).supplied.map((p) => p.value)).toEqual([
      10,
      null,
    ]);
    expect(s.leftOut).toEqual([]);
  });
});
