import { describe, expect, it } from 'vitest';

import {
  auctionStatus,
  carryForward,
  configChanges,
  isHistoryBuilding,
  metricValue,
  type HistoryPoint,
} from './lendingInsights';

const pt = (over: Partial<HistoryPoint>): HistoryPoint => ({
  t: '2026-01-01T00:00:00Z',
  removed: false,
  supplied: null,
  borrowed: null,
  utilization_pct: null,
  borrow_apr: null,
  supply_apr: null,
  collateral_factor: null,
  liability_factor: null,
  supply_cap: null,
  ...over,
});

describe('carryForward', () => {
  const day = 86_400;
  const base = Date.parse('2026-01-01T00:00:00Z') / 1000;

  it('holds a value across buckets with no point', () => {
    const out = carryForward(
      [
        { t: '2026-01-01T10:00:00Z', value: 1 },
        { t: '2026-01-04T05:00:00Z', value: 2 },
      ],
      '1d',
      '2026-01-01T00:00:00Z',
      '2026-01-05T00:00:00Z',
    );
    expect(out).toEqual([
      { time: base, value: 1 },
      { time: base + day, value: 1 },
      { time: base + 2 * day, value: 1 },
      { time: base + 3 * day, value: 2 },
      { time: base + 4 * day, value: 2 },
    ]);
  });

  it('keeps the last point of a bucket and carries an earlier state into the window', () => {
    const out = carryForward(
      [
        { t: '2025-12-30T10:00:00Z', value: 5 },
        { t: '2026-01-01T01:00:00Z', value: 6 },
        { t: '2026-01-01T09:00:00Z', value: 7 },
      ],
      '1d',
      '2026-01-01T00:00:00Z',
      '2026-01-02T00:00:00Z',
    );
    expect(out.map((p) => p.value)).toEqual([7, 7]);
  });

  it('breaks the line on a null value and returns nothing for no points', () => {
    const out = carryForward(
      [
        { t: '2026-01-01T00:00:00Z', value: 1 },
        { t: '2026-01-02T00:00:00Z', value: null },
      ],
      '1d',
      '2026-01-01T00:00:00Z',
      '2026-01-03T00:00:00Z',
    );
    expect(out.map((p) => p.value)).toEqual([1, null, null]);
    expect(carryForward([], '1d', 'a', 'b')).toEqual([]);
  });

  it('steps hourly', () => {
    const out = carryForward(
      [{ t: '2026-01-01T00:30:00Z', value: 3 }],
      '1h',
      '2026-01-01T00:00:00Z',
      '2026-01-01T02:00:00Z',
    );
    expect(out).toHaveLength(3);
  });
});

describe('metricValue', () => {
  it('scales an amount above 2^53 without parsing the raw integer', () => {
    const v = metricValue(
      pt({ supplied: '123456789012345678901234567' }),
      'supplied',
      7,
    );
    expect(v).toBe(Number('12345678901234567890.1234567'));
  });

  it('turns rates into percents and keeps nulls null', () => {
    expect(
      metricValue(pt({ borrow_apr: 0.0612 }), 'borrow_apr', 7),
    ).toBeCloseTo(6.12);
    expect(metricValue(pt({}), 'supply_apr', 7)).toBeNull();
    expect(metricValue(pt({ supplied: '5' }), 'supplied', null)).toBeNull();
    expect(
      metricValue(pt({ utilization_pct: 50, removed: true }), 'utilization', 7),
    ).toBeNull();
  });
});

describe('configChanges', () => {
  it('lists factor and cap changes between consecutive points', () => {
    const out = configChanges(
      [
        pt({
          t: '2026-01-01T00:00:00Z',
          collateral_factor: '7500000',
          liability_factor: '9000000',
          supply_cap: '1000000000',
        }),
        pt({
          t: '2026-01-02T00:00:00Z',
          collateral_factor: '8000000',
          liability_factor: '9000000',
          supply_cap: '1000000000',
        }),
        pt({
          t: '2026-01-03T00:00:00Z',
          collateral_factor: '8000000',
          liability_factor: '9000000',
          supply_cap: '2000000000',
        }),
      ],
      7,
      7,
    );
    expect(out).toEqual([
      {
        label: 'Collateral factor',
        from: '75%',
        to: '80%',
        at: '2026-01-02T00:00:00Z',
      },
      {
        label: 'Supply cap',
        from: '100',
        to: '200',
        at: '2026-01-03T00:00:00Z',
      },
    ]);
  });

  it('ignores a missing value on either side', () => {
    expect(
      configChanges(
        [
          pt({ collateral_factor: null }),
          pt({ t: '2026-01-02T00:00:00Z', collateral_factor: '8000000' }),
        ],
        7,
        7,
      ),
    ).toEqual([]);
  });
});

describe('auctionStatus', () => {
  it('derives open, partial, filled and deleted', () => {
    expect(auctionStatus({ deleted: false, fills: [] }).kind).toBe('open');
    expect(auctionStatus({ deleted: false }).kind).toBe('open');
    expect(
      auctionStatus({ deleted: false, fills: [{ fill_percent: '40' }] }),
    ).toMatchObject({ kind: 'partial', label: 'Filled 40%' });
    expect(
      auctionStatus({
        deleted: false,
        fills: [{ fill_percent: '40' }, { fill_percent: '60' }],
      }).kind,
    ).toBe('filled');
    expect(auctionStatus({ deleted: true, fills: [] }).kind).toBe('deleted');
    expect(
      auctionStatus({ deleted: true, fills: [{ fill_percent: 'x' }] }).kind,
    ).toBe('deleted');
  });
});

describe('isHistoryBuilding', () => {
  it('matches a 503 error only', () => {
    expect(isHistoryBuilding(new Error('503 Service Unavailable on /x'))).toBe(
      true,
    );
    expect(isHistoryBuilding(new Error('500 boom'))).toBe(false);
    expect(isHistoryBuilding('503')).toBe(false);
  });
});
