import { describe, expect, it } from 'vitest';

import { sumDecimalStrings } from './format';

// #569: row-sum money headlines were float reductions over served decimal
// strings; the helper sums them exactly (ADR-0003).
describe('sumDecimalStrings', () => {
  it('sums without float drift', () => {
    expect(sumDecimalStrings(['0.1', '0.2'])).toBe('0.3');
  });

  it('keeps every digit above 2^53', () => {
    expect(sumDecimalStrings(['9007199254740993.01', '1'])).toBe(
      '9007199254740994.01',
    );
  });

  it('aligns mixed scales and signs', () => {
    expect(sumDecimalStrings(['1.5', '-0.25', null, '2'])).toBe('3.25');
    expect(sumDecimalStrings(['-0.5', '0.25'])).toBe('-0.25');
  });

  it('refuses a garbage entry and an empty set', () => {
    expect(sumDecimalStrings(['1.00', 'abc'])).toBeNull();
    expect(sumDecimalStrings([null, undefined])).toBeNull();
  });
});
