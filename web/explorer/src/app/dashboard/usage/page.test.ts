import { describe, it, expect } from 'vitest';

import { fmtDateUTC } from '@/lib/account-format';

import { aggregateByDate, usageWindowUTC } from './page';
import type { UsageRow } from '@/api/account';

describe('usageWindowUTC', () => {
  it('is dense from the served window start to the newest served day, zero-filling gaps', () => {
    const rows: UsageRow[] = [
      { date: '2026-09-10', requests: 5, errors: 0, throttled: 0 },
      { date: '2026-09-15', requests: 3, errors: 0, throttled: 0 },
    ];
    const window = usageWindowUTC(rows, 30);
    expect(window[0]).toBe('2026-09-10');
    expect(window[window.length - 1]).toBe('2026-09-15');
    // dense: every day between the two served dates is present
    expect(window).toContain('2026-09-12');
    expect(window).toHaveLength(6);
  });
});

describe('aggregateByDate', () => {
  it('zero-fills a gap in served days instead of collapsing the run of bars', () => {
    const rows: UsageRow[] = [
      { date: '2026-09-10', requests: 5, errors: 1, throttled: 0 },
      { date: '2026-09-15', requests: 3, errors: 0, throttled: 2 },
    ];
    const days = aggregateByDate(rows);
    expect(days).toHaveLength(6);
    const missing = days.find((d) => d.date === '2026-09-12');
    expect(missing).toEqual({
      date: '2026-09-12',
      requests: 0,
      errors: 0,
      throttled: 0,
    });
  });

  it('returns an empty window when there are no rows', () => {
    expect(aggregateByDate([])).toEqual([]);
  });
});

describe('fmtDateUTC', () => {
  it('renders a UTC calendar-day string on the day it names, regardless of local offset', () => {
    // A date-only string parses as UTC midnight; formatting it back in a
    // negative-offset local zone (without timeZone: 'UTC') renders the
    // previous day. fmtDateUTC must pin the render to UTC.
    expect(fmtDateUTC('2026-09-17')).toBe('Sep 17, 2026');
  });
});
