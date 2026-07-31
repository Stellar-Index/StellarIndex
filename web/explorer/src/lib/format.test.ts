import { describe, it, expect } from 'vitest';

import * as format from './format';
import {
  formatPrice,
  formatCompact,
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

describe('AGT-06: dead-code percentage footgun removed', () => {
  it('does not export formatPctChange/formatLedger — every real percentage field in the app is already a percentage point, not a fraction, so a fraction-based helper is a footgun, not a utility', () => {
    expect('formatPctChange' in format).toBe(false);
    expect('formatLedger' in format).toBe(false);
  });
});

describe('formatPriceSmall / formatPairPrice', () => {
  it('keeps sub-threshold precision instead of collapsing to 0.00', () => {
    expect(formatPriceSmall(150)).toBe('150.00');
    expect(formatPriceSmall(0.0005)).toBe((0.0005).toExponential(3));
    expect(formatPairPrice(1500)).toBe('1500.00');
  });

  it('renders a real zero as the bare "0"', () => {
    expect(formatPriceSmall(0)).toBe('0');
  });

  it('COR-01: does not mask a negative price as a legitimate "0"', () => {
    // A negative price is bad data, not a zero — it must render
    // distinguishably (and not identically to a healthy zero-price row).
    expect(formatPriceSmall(-0.5)).not.toBe('0');
    expect(formatPriceSmall(-0.5)).toBe((-0.5).toExponential(3));
  });
});

describe('formatRelative', () => {
  it('never renders NaN — em-dash for missing timestamps', () => {
    expect(formatRelative(null)).toBe('—');
    expect(formatRelative(undefined)).toBe('—');
  });
});

describe('scaleBaseUnits / formatBaseUnits', () => {
  it('BigInt-divides smallest-unit integer strings past 2^53 without mis-scaling', () => {
    // 554421152474348098 stroops ≈ 5.54e17 — past 2^53, where a
    // Number()-then-divide path silently rounds the integer first.
    expect(format.formatBaseUnits('554421152474348098', 7)).toBe('55,442,115,247.4348');
    expect(format.scaleBaseUnits('554421152474348098', 7)).toBeCloseTo(55442115247.43481, 3);
  });

  it('keeps absent/garbage values as "—"/null — never NaN or a fabricated zero', () => {
    expect(format.formatBaseUnits(undefined, 7)).toBe('—');
    expect(format.formatBaseUnits('', 7)).toBe('—');
    expect(format.formatBaseUnits('not-a-number', 7)).toBe('—');
    expect(format.scaleBaseUnits(null, 7)).toBeNull();
    expect(format.scaleBaseUnits('garbage', 7)).toBeNull();
  });

  it('handles zero, negatives, and fraction trimming', () => {
    expect(format.formatBaseUnits('0', 7)).toBe('0');
    expect(format.formatBaseUnits('-25000000', 7)).toBe('-2.5');
    expect(format.formatBaseUnits('10000000000', 7)).toBe('1,000');
    expect(format.scaleBaseUnits('2500000000', 7)).toBe(250);
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
