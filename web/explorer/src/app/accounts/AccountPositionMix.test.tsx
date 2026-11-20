import { describe, it, expect } from 'vitest';

import {
  positionMixRows,
  type LendingEarnings,
} from './AccountLendingEarnings';

const earnings = (lb: boolean) =>
  ({
    pools: [
      {
        pool: 'CBLEND',
        protocol: 'blend',
        supplied_usd: '1500.129',
        borrowed_usd: '200',
        lower_bound: lb,
      },
      {
        pool: 'CBLNT',
        protocol: 'blnt',
        supplied_usd: null,
        borrowed_usd: null,
        lower_bound: false,
      },
    ],
    legs: [],
    totals: { net_apr: '0.0321' },
  }) as unknown as LendingEarnings;

describe('positionMixRows', () => {
  it('maps supplied and borrowed per pool, keeps the exact USD on hover, drops an unpriced pool', () => {
    const rows = positionMixRows(earnings(false));
    expect(rows).toHaveLength(1);
    expect(rows[0]).toMatchObject({
      a: 1500.129,
      b: 200,
      color: 'var(--color-chart-1)',
    });
    expect(rows[0].title).toContain('$1,500.12');
    expect(rows[0].aHatch).toBe(false);
  });
  it('labels a lower bound', () => {
    const [r] = positionMixRows(earnings(true));
    expect(r.aDisplay).toMatch(/^≥/);
    expect(r.aHatch).toBe(true);
  });
});
