import { describe, expect, it } from 'vitest';

import type { TradeRow } from '@/api/hooks';
import { compareTradeTsDesc, mergeRecentTrades } from './HomeRecentTrades';

function trade(ts: string, tx_hash: string): TradeRow {
  return {
    source: 'sdex',
    ledger: 1,
    tx_hash,
    op_index: 0,
    ts,
    base_asset: 'native',
    quote_asset:
      'USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN',
    base_amount: '1',
    quote_amount: '1',
    price: '1',
  };
}

describe('mergeRecentTrades', () => {
  const T = '2026-09-30T12:00:00Z';

  it('treats equal timestamps as a tie in both argument orders', () => {
    const a = trade(T, 'a');
    const b = trade(T, 'b');
    expect(compareTradeTsDesc(a, b)).toBe(0);
    expect(compareTradeTsDesc(b, a)).toBe(0);
  });

  it('orders newest first and keeps tied rows in fan-out order', () => {
    const older = trade('2026-09-30T11:59:55Z', 'old');
    const tied = ['p1', 'p2', 'p3', 'p4'].map((h) => trade(T, h));
    const merged = mergeRecentTrades(
      [[tied[0], older], [tied[1], tied[2]], [tied[3]]],
      30,
    );
    expect(merged.map((t) => t.tx_hash)).toEqual([
      'p1',
      'p2',
      'p3',
      'p4',
      'old',
    ]);
  });

  it('caps the merged feed at the limit', () => {
    const rows = ['x', 'y', 'z'].map((h) => trade(T, h));
    expect(mergeRecentTrades([rows], 2)).toHaveLength(2);
  });
});
