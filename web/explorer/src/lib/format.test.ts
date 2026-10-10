import { describe, it, expect } from 'vitest';

import * as format from './format';
import {
  compareDecimalDesc,
  formatPrice,
  formatCompact,
  formatPriceSmall,
  formatSubunitPrice,
  formatPairPrice,
  formatRelative,
  formatDurationShort,
  formatDurationLong,
  formatRelativeLong,
  formatOraclePrice,
  formatUnitsReadable,
} from './format';
import { truncateMiddle } from '@/components/ui';

// One row per case: [args, expected]. Strict equality, so null stays null.
function table<A extends unknown[]>(
  fn: (...a: A) => unknown,
  rows: [A, unknown][],
) {
  it.each(rows)('%j -> %j', (args, want) => {
    expect(fn(...args)).toBe(want);
  });
}

describe('formatPrice', () => {
  table(formatPrice, [
    [[1234.5], '1,234.50'],
    [[0], '0.00'],
    [['42'], '42.00'],
    [['not-a-number'], '—'],
    [[Infinity], '—'],
  ]);
});

describe('formatCompact', () => {
  table(formatCompact, [
    [[1_500_000], '1.5M'],
    [[2_000], '2K'],
    [['x'], '—'],
  ]);
});

describe('AGT-06: dead-code percentage footgun removed', () => {
  it('does not export formatPctChange/formatLedger — a fraction-based percentage helper is a footgun, every real percentage field is already a percentage point', () => {
    expect('formatPctChange' in format).toBe(false);
    expect('formatLedger' in format).toBe(false);
  });
});

describe('formatPriceSmall', () => {
  table(formatPriceSmall, [
    [[150], '150.00'],
    [[0.0005], '0.0005'],
    [[0], '0'],
    // COR-01: a negative price is bad data and must not render as a healthy zero.
    [[-0.5], '-0.5'],
    // The exact decimal is rounded, not its nearest double (2.0000499999…).
    [['2.00005'], '2.0001'],
    [['150'], '150.00'],
    [['0.0123456'], '0.012346'],
    [['0.0005'], '0.0005'],
    [['0'], '0'],
    [['-0.5'], '-0.5'],
    [[1e-25], '<0.00000000000000000001'],
    [[-1e-25], '-<0.00000000000000000001'],
  ]);

  it('never emits an exponent for tiny prices', () => {
    for (const n of [3.353e-4, 1e-6, 9.9e-9, 2.5e-11]) {
      expect(formatPriceSmall(n)).not.toMatch(/e/i);
    }
  });
});

describe('formatPairPrice', () => {
  table(formatPairPrice, [
    [[1500], '1500.00'],
    [['2.00005'], '2.0001'],
    [['12345678901234567.89'], '12345678901234567.89'],
    [['0.00001234567'], '0.00001235'],
    [['0.000000000000000000001'], '<0.00000000000000000001'],
    [['abc'], '—'],
  ]);
});

describe('formatOraclePrice', () => {
  // An oracle reading is evidence: unparseable values are reported verbatim,
  // and a blank is absence, not a zero quote (Number('') is 0).
  table(formatOraclePrice, [
    [['1.00003382630191'], '1.0000'],
    [['0.999822000'], '0.999822'],
    [['0.17892015847842'], '0.178920'],
    [['0.0000034521'], '0.000003452'],
    [['0'], '0'],
    [['0.00000000'], '0'],
    [['not-a-number'], 'not-a-number'],
    [[''], ''],
    [['   '], '   '],
  ]);
});

describe('formatRelative', () => {
  table(formatRelative, [
    [[null], '—'],
    [[undefined], '—'],
  ]);

  it('drops the suffix for dense feeds when asked', () => {
    const iso = new Date(Date.now() - 3 * 3600_000).toISOString();
    expect(formatRelative(iso)).toBe('3h ago');
    expect(formatRelative(iso, { suffix: false })).toBe('3h');
  });
});

describe('formatBaseUnits', () => {
  // 554421152474348098 is past 2^53, where Number()-then-divide mis-scales.
  table(format.formatBaseUnits, [
    [['554421152474348098', 7], '55,442,115,247.4348'],
    [[undefined, 7], '—'],
    [['', 7], '—'],
    [['not-a-number', 7], '—'],
    [['0', 7], '0'],
    [['-25000000', 7], '-2.5'],
    [['10000000000', 7], '1,000'],
  ]);
});

describe('scaleBaseUnits', () => {
  table(format.scaleBaseUnits, [
    [[null, 7], null],
    [['garbage', 7], null],
    [['2500000000', 7], 250],
  ]);

  it('BigInt-divides past 2^53', () => {
    expect(format.scaleBaseUnits('554421152474348098', 7)).toBeCloseTo(
      55442115247.43481,
      3,
    );
  });
});

describe('baseUnitsDecimal', () => {
  table(format.baseUnitsDecimal, [
    [['554421152474348098', 7], '55442115247.4348098'],
    [['5', 7], '0.0000005'],
    [['-25000000', 7], '-2.5000000'],
    [['42', 0], '42'],
    [[undefined, 7], null],
    [['', 7], null],
    [['1.5', 7], null],
    [['1e9', 7], null],
    [['100', -1], null],
    [['100', 1.5], null],
  ]);

  it('scales exactly, so rounding the result is exact too', () => {
    // As a float, 2.00005 sits just below the half and rounds to 2.0000.
    expect(formatPriceSmall(format.baseUnitsDecimal('200005000', 8)!)).toBe(
      '2.0001',
    );
  });
});

describe('formatDecimalAmount', () => {
  // 9007199254740993 = 2^53 + 1: a parse-first formatter is off before it starts.
  table(format.formatDecimalAmount, [
    [['40538494.54'], '40,538,494.54'],
    [['1569.77'], '1,569.77'],
    [['0.01'], '0.01'],
    [['1000'], '1,000.00'],
    [['1000.5'], '1,000.50'],
    [['-12.30'], '-12.30'],
    [['9007199254740993.07'], '9,007,199,254,740,993.07'],
    [['123456789012345678901234.99'], '123,456,789,012,345,678,901,234.99'],
    [[undefined], null],
    [[null], null],
    [[''], null],
    [['NaN'], null],
    [['1e6'], null],
  ]);
});

describe('truncateMiddle', () => {
  it('keeps short strings whole', () => {
    expect(truncateMiddle('short')).toBe('short');
  });
  it('truncates long identifiers to head…tail', () => {
    expect(truncateMiddle('GABCDEFGHIJKLMNOP', 6, 4)).toBe('GABCDE…MNOP');
  });
});

// No scientific notation anywhere a price renders.
describe('formatSubunitPrice', () => {
  table(formatSubunitPrice, [
    [[3.353e-4], '0.0003353'],
    [[1.234e-7], '0.0000001234'],
    [[0.0005], '0.0005'],
    [[-3.353e-4], '-0.0003353'],
    // Decimal tail is capped at 20 places.
    [[1e-18], '0.000000000000000001'],
    [[1e-20], '0.00000000000000000001'],
    // Dust below the cap is a signed bound, never "0" or "-0".
    [[1e-25], '<0.00000000000000000001'],
    [[-1e-25], '-<0.00000000000000000001'],
  ]);
});

describe('formatDurationShort', () => {
  table(formatDurationShort, [
    [[45], '45s'],
    [[180], '3m'],
    [[7200], '2h'],
    [[200000], '2d'],
    [[-5], '—'],
    [[Number.NaN], '—'],
  ]);
});

describe('formatDurationLong', () => {
  table(formatDurationLong, [
    [[135 * 60_000], '2h 15m'],
    [[30 * 60_000], '30m'],
    [[120 * 60_000], '2h'],
    [[Number.NaN], '—'],
  ]);
});

describe('formatRelativeLong', () => {
  it('renders word-form buckets', () => {
    const iso = new Date(Date.now() - 2 * 3600_000).toISOString();
    expect(formatRelativeLong(iso)).toBe('2 hours ago');
    expect(formatRelativeLong(null)).toBe('never');
  });
});

describe('multiplyDecimalStrings', () => {
  table(format.multiplyDecimalStrings, [
    [['0.000000001', '0.0003'], '0.0000000000003'],
    [['2.50', '4'], '10'],
    [['123456789012345678901', '1.5'], '185185183518518518351.5'],
    [['-0.5', '0.25'], '-0.125'],
    [['-0.5', '-0.25'], '0.125'],
    [['-0.5', '0.00'], '0'],
    [['1e-9', '1'], null],
    [['NaN', '1'], null],
    [['', '1'], null],
  ]);
});

describe('formatCompactUnits', () => {
  // Rounds the exact value, not a float that crossed the display boundary.
  table(format.formatCompactUnits, [
    [['5123049999999999660566', 7], '512.3T'],
    [['8030049999999999577453', 7], '803T'],
    [['1000000000000000000000000000', 18], '1B'],
    [['1250.0000000'], '1.25K'],
    [['1234.5'], '1.23K'],
    [['12.345'], '12.35'],
    [['-12.345'], '-12.35'],
    [['7'], '7'],
    [['123456', 7], '0.01'],
    [[undefined], '—'],
    [[''], '—'],
    [['1e27'], '—'],
  ]);
});

describe('decimalOrNull', () => {
  table(format.decimalOrNull, [
    [['12.5'], 12.5],
    [['0'], 0],
    [[undefined], null],
    [[null], null],
    [[''], null],
    [['n/a'], null],
  ]);
});

describe('compareDecimalDesc', () => {
  table(compareDecimalDesc, [
    [[null, ''], 0],
    [['1', null], -1],
    [['x', '1'], 1],
  ]);

  it('orders exactly above 2^53 and ranks malformed rows as 0', () => {
    const rows = ['9007199254740992', null, '9007199254740993', ''];
    expect([...rows].sort(compareDecimalDesc)[0]).toBe('9007199254740993');
    expect(['1', 'x', '5'].sort(compareDecimalDesc)).toEqual(['5', '1', 'x']);
  });
});

describe('formatUsdWhole', () => {
  table(format.formatUsdWhole, [
    [['9007199254740993.5'], '$9,007,199,254,740,994'],
    [['1234.49'], '$1,234'],
    [['-1234.5'], '-$1,235'],
    [[null], '—'],
    [['1e5'], '—'],
  ]);
});

describe('formatReadable', () => {
  table(format.formatReadable, [
    [['11262451732.01', true], '$11.26B'],
    [['-11262451732.01', true], '-$11.26B'],
    [['1234567'], '1.23M'],
    [['12345.67'], '12,346'],
    [['999.999', true], '$1K'],
    [['12.345'], '12.35'],
    [['0.000123456'], '0.0001235'],
    [['0'], '0'],
    [['USDC'], null],
    [[null], null],
  ]);
});

describe('formatWhole', () => {
  table(format.formatWhole, [
    [['9007199254740993.5'], '9,007,199,254,740,994'],
    [['-1234.5'], '-1,235'],
    [['-0.4'], '0'],
    [[null], '—'],
    [['1e5'], '—'],
  ]);
});

describe('divideDecimalString', () => {
  table(format.divideDecimalString, [
    [['18014398509481986', 2, 0], '9007199254740993'],
    [['10.0000005', 2, 2], '5.0000002'],
    [['10', 0], null],
    [['abc', 2], null],
  ]);
});

describe('formatUnitsReadable', () => {
  it.each([
    ['123456789', 7, '12.35'],
    ['1000000000000000', 7, '100M'],
    ['9007199254740993', 0, '9007.2T'],
    [null, 7, '—'],
  ] as const)('%s at %i decimals → %s', (raw, d, want) => {
    expect(formatUnitsReadable(raw, d)).toBe(want);
  });
});
