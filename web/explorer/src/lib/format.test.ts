import { describe, it, expect } from 'vitest';

import {
  formatPrice,
  formatCompact,
  formatPctChange,
  formatLedger,
  formatPriceSmall,
  formatPairPrice,
  formatRelative,
} from './format';
import { truncateMiddle } from '@/components/ui';

describe('formatPrice', () => {
  it('formats with 2–8 fraction digits and grouping', () => {
    expect(formatPrice(1234.5)).toBe('1,234.50');
    expect(formatPrice(0)).toBe('0.00');
  });
  it('parses numeric strings', () => {
    expect(formatPrice('42')).toBe('42.00');
  });
  it('returns an em-dash for non-finite input', () => {
    expect(formatPrice('not-a-number')).toBe('—');
    expect(formatPrice(Infinity)).toBe('—');
  });
});

describe('formatCompact', () => {
  it('uses compact notation', () => {
    expect(formatCompact(1_500_000)).toBe('1.5M');
    expect(formatCompact(2_000)).toBe('2K');
  });
  it('returns an em-dash for junk', () => {
    expect(formatCompact('x')).toBe('—');
  });
});

describe('formatPctChange', () => {
  it('signs (except zero) and suffixes %', () => {
    expect(formatPctChange(0.0123)).toBe('+1.23%');
    expect(formatPctChange(-0.05)).toBe('-5.00%');
    expect(formatPctChange(0)).toBe('0.00%');
  });
  it('returns an em-dash for NaN', () => {
    expect(formatPctChange(NaN)).toBe('—');
  });
});

describe('formatLedger', () => {
  it('prefixes # and groups digits', () => {
    expect(formatLedger(1_234_567)).toBe('#1,234,567');
  });
});

describe('formatPriceSmall / formatPairPrice', () => {
  it('keeps sub-threshold precision instead of collapsing to 0.00', () => {
    expect(formatPriceSmall(150)).toBe('150.00');
    expect(formatPriceSmall(0.0005)).toBe((0.0005).toExponential(3));
    expect(formatPairPrice(1500)).toBe('1500.00');
  });
});

describe('formatRelative', () => {
  it('never renders NaN — em-dash for missing timestamps', () => {
    expect(formatRelative(null)).toBe('—');
    expect(formatRelative(undefined)).toBe('—');
  });
});

describe('truncateMiddle', () => {
  it('keeps short strings whole', () => {
    expect(truncateMiddle('short')).toBe('short');
  });
  it('truncates long identifiers to head…tail', () => {
    expect(truncateMiddle('GABCDEFGHIJKLMNOP', 6, 4)).toBe('GABCDE…MNOP');
  });
});
