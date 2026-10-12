import { describe, it, expect } from 'vitest';

import {
  coverageDetail,
  coverageIncomplete,
  formatMovementAmount,
} from './AccountMovements';

describe('formatMovementAmount', () => {
  it('scales an 18-decimal token by its own decimals, not a feed-wide 7', () => {
    expect(formatMovementAmount('1000000000000000000', 18)).toBe('1');
  });

  it('scales a 6-decimal token by its own decimals', () => {
    expect(formatMovementAmount('2500000', 6)).toBe('2.5');
  });

  it('rounds a 7-decimal row for reading', () => {
    expect(formatMovementAmount('12345678', 7)).toBe('1.23');
  });

  it('shows labelled base units when the scale is unknown', () => {
    expect(formatMovementAmount('1000000000000000000', undefined)).toBe(
      '1,000,000,000,000,000,000 base units',
    );
  });
});

describe('coverageDetail', () => {
  it('points fees at the per-transaction fee column, not a gap', () => {
    expect(coverageDetail({ kind: 'fee', status: 'not_served' })).toBe(
      'shown per transaction',
    );
  });

  it('points order-book fills at the Trades panel, not a gap', () => {
    expect(coverageDetail({ kind: 'fill', status: 'not_served' })).toBe(
      'shown in Trades',
    );
  });
});

describe('coverageIncomplete', () => {
  it('warns only for a kind this feed serves that is not complete', () => {
    expect(coverageIncomplete({ kind: 'transfer', status: 'served' })).toBe(
      false,
    );
    expect(coverageIncomplete({ kind: 'fee', status: 'not_served' })).toBe(
      false,
    );
    expect(coverageIncomplete({ kind: 'fill', status: 'not_served' })).toBe(
      false,
    );
    expect(
      coverageIncomplete({ kind: 'mint_burn_clawback', status: 'partial' }),
    ).toBe(true);
    expect(
      coverageIncomplete({ kind: 'mint_burn_clawback', status: 'not_served' }),
    ).toBe(true);
    expect(coverageIncomplete({ kind: 'transfer', status: 'partial' })).toBe(
      true,
    );
  });
});
