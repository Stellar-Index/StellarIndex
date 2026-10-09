import { describe, expect, it } from 'vitest';

import { topSlices } from './topSlices';

describe('topSlices', () => {
  it('keeps the largest and sums the rest into Other exactly above 2^53', () => {
    const out = topSlices(
      [
        { label: 'A', value: 9e15, decimal: '9007199254740993' },
        { label: 'B', value: 5, decimal: '5' },
        { label: 'C', value: 3, decimal: '9007199254740993' },
        { label: 'D', value: 1, decimal: '1' },
      ],
      1,
    );
    expect(out.map((s) => s.label)).toEqual(['A', 'Other (3)']);
    expect(out[1].decimal).toBe('9007199254740999');
  });

  it('returns the input sorted when it fits', () => {
    const out = topSlices(
      [
        { label: 'x', value: 1, decimal: '1' },
        { label: 'y', value: 2, decimal: '2' },
      ],
      3,
    );
    expect(out.map((s) => s.label)).toEqual(['y', 'x']);
  });
});
