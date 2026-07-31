import { describe, it, expect } from 'vitest';

import {
  dropPartialTrailingDay,
  isPartialTodayDate,
  seriesPointTime,
  todayUTC,
} from './series';

describe('dropPartialTrailingDay', () => {
  const now = new Date('2026-07-31T09:00:00Z');

  it("drops a trailing point stamped with today's UTC day (partial bucket)", () => {
    const pts = [
      { date: '2026-07-29', value: '1' },
      { date: '2026-07-30', value: '2' },
      { date: '2026-07-31', value: '3' },
    ];
    expect(dropPartialTrailingDay(pts, now).map((p) => p.date)).toEqual([
      '2026-07-29',
      '2026-07-30',
    ]);
  });

  it('keeps a series that already ends on a complete day', () => {
    const pts = [{ date: '2026-07-29' }, { date: '2026-07-30' }];
    expect(dropPartialTrailingDay(pts, now)).toEqual(pts);
  });

  it('keeps hourly (intraday) points — the 24h window shows the live day', () => {
    const pts = [{ date: '2026-07-31T08:00' }, { date: '2026-07-31T09:00' }];
    expect(dropPartialTrailingDay(pts, now)).toEqual(pts);
  });

  it('handles empty series', () => {
    expect(dropPartialTrailingDay([], now)).toEqual([]);
  });
});

describe('isPartialTodayDate / todayUTC', () => {
  const now = new Date('2026-07-31T23:59:00Z');
  it('flags only the daily-grain stamp for the current UTC day', () => {
    expect(todayUTC(now)).toBe('2026-07-31');
    expect(isPartialTodayDate('2026-07-31', now)).toBe(true);
    expect(isPartialTodayDate('2026-07-30', now)).toBe(false);
    expect(isPartialTodayDate('2026-07-31T12:00', now)).toBe(false);
    expect(isPartialTodayDate(null, now)).toBe(false);
  });
});

describe('seriesPointTime', () => {
  it('parses daily and hourly stamps as UTC unix seconds', () => {
    expect(seriesPointTime('2026-07-30')).toBe(Date.parse('2026-07-30T00:00:00Z') / 1000);
    expect(seriesPointTime('2026-07-30T13:00')).toBe(
      Date.parse('2026-07-30T13:00:00Z') / 1000,
    );
  });

  it('returns null — never an epoch-0 fallback — for a non-parsable date', () => {
    expect(seriesPointTime('')).toBeNull();
    expect(seriesPointTime('not-a-date-at-all-xx')).toBeNull();
  });
});
