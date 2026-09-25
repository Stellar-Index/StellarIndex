import { describe, it, expect } from 'vitest';

import { formatMovementAmount } from './AccountMovements';

describe('formatMovementAmount', () => {
  it('scales an 18-decimal token by its own decimals, not a feed-wide 7', () => {
    expect(formatMovementAmount('1000000000000000000', 18)).toBe('1');
  });

  it('scales a 6-decimal token by its own decimals', () => {
    expect(formatMovementAmount('2500000', 6)).toBe('2.5');
  });

  it('keeps the stroop rendering for 7-decimal rows', () => {
    expect(formatMovementAmount('12345678', 7)).toBe('1.2345678');
  });

  it('shows labelled base units when the scale is unknown', () => {
    expect(formatMovementAmount('1000000000000000000', undefined)).toBe(
      '1,000,000,000,000,000,000 base units',
    );
  });
});
