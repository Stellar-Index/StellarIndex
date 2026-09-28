import { describe, it, expect } from 'vitest';

import { buildTradeViz } from './AccountTrades';
import { buildMovementFlow } from './AccountMovements';
import type { components } from '@/api/types';

type AccountTrade = components['schemas']['AccountTrade'];
type AccountMovement = components['schemas']['AccountMovement'];

function trade(over: Partial<AccountTrade>): AccountTrade {
  return {
    ts: '2026-07-29T12:00:00Z',
    source: 'sdex',
    base_asset: 'native',
    quote_asset: 'USDC-G',
    base_amount: '1',
    quote_amount: '1',
    usd_volume: '1.00',
    tx_hash: 'h',
    ledger: 1,
    op_index: 0,
    role: 'taker',
    ...over,
  } as AccountTrade;
}

describe('buildTradeViz', () => {
  it('accumulates USD over priced trades only and folds venues', () => {
    const { cumulative, venues, pricedCount, totalUsd } = buildTradeViz([
      trade({
        ts: '2026-07-29T12:00:02Z',
        usd_volume: '3.00',
        source: 'soroswap',
      }),
      trade({ ts: '2026-07-29T12:00:01Z', usd_volume: '2.00' }),
      // Unpriced — unknown, never $0; must not appear in either chart.
      trade({ ts: '2026-07-29T12:00:03Z', usd_volume: undefined }),
    ]);
    expect(pricedCount).toBe(2);
    // Ascending time, cumulative sum (plotted geometry — float is fine).
    expect(cumulative.map((p) => p.value)).toEqual([2, 5]);
    expect(cumulative[0].time).toBeLessThan(cumulative[1].time);
    // The displayed $ figures (venue totals, grand total) are exact sums
    // of the served usd_volume strings, not the re-floated cumulative.
    expect(venues).toEqual(
      expect.arrayContaining([
        { label: 'sdex', value: 2, decimal: '2.00' },
        { label: 'soroswap', value: 3, decimal: '3.00' },
      ]),
    );
    expect(totalUsd).toBe('5.00');
  });

  it('folds same-second trades into one strictly-ascending point (lightweight-charts contract)', () => {
    const { cumulative } = buildTradeViz([
      trade({ ts: '2026-07-29T12:00:01Z', usd_volume: '2.00' }),
      trade({ ts: '2026-07-29T12:00:01Z', usd_volume: '3.00' }),
    ]);
    expect(cumulative).toHaveLength(1);
    expect(cumulative[0].value).toBe(5);
  });

  it('sums a venue and the grand total exactly above 2^53, where float addition drifts', () => {
    const { venues, totalUsd } = buildTradeViz([
      trade({ ts: '2026-07-29T12:00:00Z', usd_volume: '9007199254740993' }),
      trade({ ts: '2026-07-29T12:00:01Z', usd_volume: '9007199254740995' }),
      trade({
        ts: '2026-07-29T12:00:02Z',
        source: 'aquarius',
        usd_volume: '9007199254740997',
      }),
    ]);
    const sdex = venues.find((v) => v.label === 'sdex');
    expect(sdex?.decimal).toBe('18014398509481988');
    // The naive float reduce over the same three rows lands one short
    // (27021597764222984) — the exact BigInt sum is what must render.
    expect(totalUsd).toBe('27021597764222985');
  });
});

function movement(over: Partial<AccountMovement>): AccountMovement {
  return {
    ledger: 1,
    ledger_close_time: '2026-07-29T10:00:00Z',
    tx_hash: 'h',
    op_index: 0,
    leg_index: 0,
    movement_kind: 'transfer',
    direction: 'received',
    asset: 'native',
    amount: '1',
    provenance: 'recent_tail',
    ...over,
  } as AccountMovement;
}

describe('buildMovementFlow', () => {
  it('buckets in/out counts per UTC day, oldest first, ignoring self legs', () => {
    const flow = buildMovementFlow([
      movement({
        ledger_close_time: '2026-07-30T01:00:00Z',
        direction: 'sent',
      }),
      movement({
        ledger_close_time: '2026-07-29T10:00:00Z',
        direction: 'received',
      }),
      movement({
        ledger_close_time: '2026-07-29T11:00:00Z',
        direction: 'received',
      }),
      movement({
        ledger_close_time: '2026-07-29T12:00:00Z',
        direction: 'self',
      }),
    ]);
    expect(flow).toEqual([
      { label: 'Jul 29', pos: 2, neg: 0 },
      { label: 'Jul 30', pos: 0, neg: 1 },
    ]);
  });
});
