import { describe, expect, it } from 'vitest';

import { multiplePath, widestDelta } from './DivergenceMultiples';

describe('widestDelta', () => {
  it('keeps the sign of the reference furthest from our VWAP, null where none compared', () => {
    expect(
      widestDelta([
        {
          t: 'a',
          references: [
            { reference: 'x', delta_pct: '0.5' },
            { reference: 'y', delta_pct: '-1.2' },
          ],
        },
        { t: 'b', references: [] },
        { t: 'c', references: [{ reference: 'x', delta_pct: 'nope' }] },
      ] as never),
    ).toEqual([-1.2, null, null]);
  });
});

describe('multiplePath', () => {
  it('breaks the line at a gap and clamps to the bound', () => {
    expect(multiplePath([1, null, -5, 0], 2)).toBe(
      'M0.0,12.0M106.7,48.0L160.0,24.0',
    );
  });

  it('draws nothing without a usable bound', () => {
    expect(multiplePath([1, 2], 0)).toBe('');
  });
});
