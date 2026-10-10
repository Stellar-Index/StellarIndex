import { describe, expect, it } from 'vitest';

import { lagBarPct } from './CursorsTable';

describe('lagBarPct', () => {
  it('fills the largest lag and log-scales the rest', () => {
    expect(lagBarPct(999, 999)).toBe(100);
    expect(lagBarPct(9, 999)).toBeCloseTo(33.33, 1);
  });

  it.each([
    [0, 100],
    [5, 0],
    [Number.NaN, 100],
    [-3, 100],
  ])('draws no bar for lag %s against max %s', (s, max) => {
    expect(lagBarPct(s, max)).toBe(0);
  });
});
