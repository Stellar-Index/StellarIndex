import { describe, it, expect } from 'vitest';

import { borrowRate, rateCurve, type RateModel } from './blendRate';

// The reference config from the pool contract's own calc_accrual tests.
const ref: RateModel = {
  target: 7_500_000n,
  rBase: 100_000n,
  rOne: 500_000n,
  rTwo: 5_000_000n,
  rThree: 15_000_000n,
};
const ONE = 10_000_000n;

describe('borrowRate', () => {
  it.each([
    ['under target', 6_565_656n, 537_711n],
    ['over target', 7_979_797n, 1_799_493n],
    ['over 95%', 9_700_000n, 11_600_000n],
  ])('%s', (_n, util, want) => {
    expect(borrowRate(ref, util, ONE)).toBe(want);
  });

  it('a zero target at zero utilisation returns the base rate', () => {
    expect(borrowRate({ ...ref, target: 0n }, 0n, ONE)).toBe(ref.rBase);
  });

  it('scales with the rate modifier', () => {
    expect(borrowRate(ref, 0n, 2n * ONE)).toBe(2n * ref.rBase);
  });
});

describe('rateCurve', () => {
  it('is sorted, spans 0 to 100% and includes the kinks', () => {
    const c = rateCurve(ref, ONE);
    expect(c[0].util).toBe(0n);
    expect(c[c.length - 1].util).toBe(ONE);
    const utils = c.map((p) => p.util);
    expect(utils).toContain(7_500_000n);
    expect(utils).toContain(9_500_000n);
    for (let i = 1; i < c.length; i++) {
      expect(c[i].util > c[i - 1].util).toBe(true);
      expect(c[i].rate >= c[i - 1].rate).toBe(true);
    }
  });
});
