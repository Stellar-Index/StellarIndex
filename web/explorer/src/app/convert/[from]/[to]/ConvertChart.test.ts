import { describe, it, expect } from 'vitest';

import { rateRange } from './ConvertChart';

describe('rateRange', () => {
  it('finds high, low and first-to-last change exactly', () => {
    expect(rateRange(['1.00', '1.20', '0.90', '1.10'])).toEqual({
      lo: '0.90',
      hi: '1.20',
      change: 10,
    });
  });

  it('is null for no points', () => {
    expect(rateRange([])).toBeNull();
  });
});
