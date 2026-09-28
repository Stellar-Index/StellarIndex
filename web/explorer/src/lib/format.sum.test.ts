import { describe, expect, it } from 'vitest';

import { changePct, ratioPct, sumDecimalStrings } from './format';

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

describe('ratioPct / changePct', () => {
  // A whole above 2^53 at a rounding boundary: the exact 1.005% rounds to
  // 1.01; the float quotient lands at 1.00499… and rounds to 1.00.
  it('rounds the exact quotient where float rounding is wrong above 2^53', () => {
    const [part, whole] = ['100500000000000', '10000000000000000'];
    expect(Number(((Number(part) / Number(whole)) * 100).toFixed(2))).toBe(1);
    expect(ratioPct(part, whole)).toBe(1.01);
  });

  it('computes signed change from decimal strings', () => {
    expect(changePct('0.1', '0.3')).toBe(200);
    expect(changePct('2', '1.5')).toBe(-25);
    expect(changePct('-2', '-1')).toBe(-50);
    expect(changePct('9007199254740993', '9007199254740993')).toBe(0);
  });

  it('refuses a zero base and garbage', () => {
    expect(ratioPct('1', '0')).toBeNull();
    expect(changePct('0', '1')).toBeNull();
    expect(ratioPct('abc', '1')).toBeNull();
    expect(ratioPct(null, '1')).toBeNull();
  });
});
