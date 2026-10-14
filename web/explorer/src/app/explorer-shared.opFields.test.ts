import { describe, expect, it } from 'vitest';

import {
  opSucceeded,
  renderOpFieldValue,
  stroopsToXlmFixed,
} from './explorer-shared';

describe('stroopsToXlmFixed', () => {
  it('keeps seven fractional digits so fee columns align by place', () => {
    expect(stroopsToXlmFixed('100')).toBe('0.0000100');
    expect(stroopsToXlmFixed('12345678')).toBe('1.2345678');
    expect(stroopsToXlmFixed('10000000')).toBe('1.0000000');
    expect(stroopsToXlmFixed(null)).toBe('—');
  });
});

describe('opSucceeded', () => {
  it('treats result_code 0 as opINNER, decided by the inner code', () => {
    expect(
      opSucceeded({ result_code: 0, inner_result: 'payment_success' }),
    ).toBe(true);
    expect(
      opSucceeded({
        result_code: 0,
        inner_result: 'path_payment_strict_receive_over_sendmax',
      }),
    ).toBe(false);
    expect(opSucceeded({ result_code: 0 })).toBe(true);
    expect(opSucceeded({ result_code: -1 })).toBe(false);
    expect(opSucceeded({ result_code: undefined })).toBeUndefined();
    expect(opSucceeded({})).toBeUndefined();
  });
});

describe('renderOpFieldValue price', () => {
  it('renders n/d as a decimal to one stroop, trailing zeros trimmed', () => {
    expect(renderOpFieldValue('price', { n: 10009651, d: 10000000 })).toBe(
      '1.0009651',
    );
    expect(renderOpFieldValue('price', { n: '400009', d: '400000' })).toBe(
      '1.0000225',
    );
    expect(renderOpFieldValue('price', { n: 1, d: 3 })).toBe('0.3333333');
    expect(renderOpFieldValue('price', { n: 5, d: 1 })).toBe('5');
    expect(renderOpFieldValue('price', { n: 1, d: 0 })).toBe('1 / 0');
  });
  it('leaves other objects and keys as JSON', () => {
    expect(renderOpFieldValue('price', { x: 1 })).toBe('{"x":1}');
    expect(renderOpFieldValue('limit', { n: 1, d: 2 })).toBe('{"n":1,"d":2}');
  });
});
