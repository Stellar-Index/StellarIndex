import { describe, expect, it } from 'vitest';

import { scaledUnits } from './explorer-shared';

describe('scaledUnits', () => {
  it('parses a >2^53 integer at decimals 0 to the nearest double of the exact value', () => {
    // 2^53 + 1 has no exact double; it must round to 2^53 and no further.
    expect(scaledUnits('9007199254740993', 0)).toBe(2 ** 53);
    expect(scaledUnits('-9007199254740993', 0)).toBe(-(2 ** 53));
    expect(scaledUnits('170141183460469231731687303715884105727', 0)).toBe(
      Number('170141183460469231731687303715884105727'),
    );
  });

  it('scales up by 10^|decimals| for negative decimals', () => {
    expect(scaledUnits('12', -2)).toBe(1200);
    expect(scaledUnits('-12', -2)).toBe(-1200);
    expect(scaledUnits('9007199254740993', -3)).toBe(
      Number('9007199254740993000'),
    );
  });

  it('scales down by 10^decimals on the string, not by float division', () => {
    expect(scaledUnits('12345678', 7)).toBe(1.2345678);
    expect(scaledUnits('-5', 7)).toBe(-0.0000005);
    expect(scaledUnits('12345678901234567890123', 7)).toBe(
      Number('1234567890123456.7890123'),
    );
  });

  it('returns NaN for non-integer input', () => {
    expect(scaledUnits('1.5', 7)).toBeNaN();
    expect(scaledUnits('abc', 0)).toBeNaN();
  });
});
