import { describe, it, expect } from 'vitest';

import * as format from './format';
import {
  formatPrice,
  formatCompact,
  formatPriceSmall,
  formatSubunitPrice,
  formatPairPrice,
  formatRelative,
  formatDurationShort,
  formatDurationLong,
  formatRelativeLong,
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
    expect(formatPriceSmall(0.0005)).toBe('0.0005'); // plain decimal since 2026-08-06
    expect(formatPairPrice(1500)).toBe('1500.00');
  });

  it('renders a real zero as the bare "0"', () => {
    expect(formatPriceSmall(0)).toBe('0');
  });

  it('COR-01: does not mask a negative price as a legitimate "0"', () => {
    // A negative price is bad data, not a zero — it must render
    // distinguishably (and not identically to a healthy zero-price row).
    expect(formatPriceSmall(-0.5)).not.toBe('0');
    expect(formatPriceSmall(-0.5)).toBe('-0.5'); // plain decimal since 2026-08-06
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
    expect(format.formatBaseUnits('554421152474348098', 7)).toBe(
      '55,442,115,247.4348',
    );
    expect(format.scaleBaseUnits('554421152474348098', 7)).toBeCloseTo(
      55442115247.43481,
      3,
    );
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

// 2026-08-06 operator call: no scientific notation anywhere a price
// renders — "$3.353e-4" is not user-friendly; "$0.0003353" is no less
// accurate.
describe('formatSubunitPrice', () => {
  it('renders the founding example as a plain decimal', () => {
    expect(formatSubunitPrice(3.353e-4)).toBe('0.0003353');
  });
  it('keeps 4 significant digits however deep the leading zeros', () => {
    expect(formatSubunitPrice(1.234e-7)).toBe('0.0000001234');
  });
  it('trims trailing zeros', () => {
    expect(formatSubunitPrice(0.0005)).toBe('0.0005');
  });
  it('keeps the bad-data negative sign visible (COR-01)', () => {
    expect(formatSubunitPrice(-3.353e-4)).toBe('-0.0003353');
  });
  it('caps the decimal tail at 20 places for deep dust', () => {
    // 1e-18 still renders as an honest plain decimal within the cap —
    // long, but accurate, and monospace columns absorb it.
    expect(formatSubunitPrice(1e-18)).toBe('0.000000000000000001');
  });
});

describe('formatPriceSmall — no scientific notation', () => {
  it('never emits an exponent for tiny prices', () => {
    for (const n of [3.353e-4, 1e-6, 9.9e-9, 2.5e-11]) {
      expect(formatPriceSmall(n)).not.toMatch(/e/i);
    }
  });
});

// FEC audit A3-F1/F1b: the consolidated relative/duration canonicals.
describe('formatDurationShort', () => {
  it('formats second buckets without a suffix', () => {
    expect(formatDurationShort(45)).toBe('45s');
    expect(formatDurationShort(180)).toBe('3m');
    expect(formatDurationShort(7200)).toBe('2h');
    expect(formatDurationShort(200000)).toBe('2d');
  });
  it('renders negative (clock-skewed) and non-finite lags as unknown', () => {
    expect(formatDurationShort(-5)).toBe('—');
    expect(formatDurationShort(Number.NaN)).toBe('—');
  });
});

describe('formatDurationLong', () => {
  it('formats compound durations', () => {
    expect(formatDurationLong(135 * 60_000)).toBe('2h 15m');
    expect(formatDurationLong(30 * 60_000)).toBe('30m');
    expect(formatDurationLong(120 * 60_000)).toBe('2h');
  });
  it('guards non-finite input (previously rendered "NaNm")', () => {
    expect(formatDurationLong(Number.NaN)).toBe('—');
  });
});

describe('formatRelative suffix option', () => {
  it('drops the suffix for dense feeds when asked', () => {
    const iso = new Date(Date.now() - 3 * 3600_000).toISOString();
    expect(formatRelative(iso)).toBe('3h ago');
    expect(formatRelative(iso, { suffix: false })).toBe('3h');
  });
});

describe('formatRelativeLong', () => {
  it('renders word-form buckets', () => {
    const iso = new Date(Date.now() - 2 * 3600_000).toISOString();
    expect(formatRelativeLong(iso)).toBe('2 hours ago');
    expect(formatRelativeLong(null)).toBe('never');
  });
});
