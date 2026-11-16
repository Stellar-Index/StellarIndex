import { describe, it, expect } from 'vitest';

import {
  changeValues,
  configKindLabel,
  fixedDecimal,
  fixedPct,
  poolStatusText,
  supplyCapText,
} from './poolConfig';

describe('fixedPct', () => {
  it('scales by the response decimals', () => {
    expect(fixedPct('9500000', 7)).toBe('95%');
    expect(fixedPct('7500000', 7)).toBe('75%');
    expect(fixedPct('1250000', 7)).toBe('12.5%');
    expect(fixedPct('1234567', 7)).toBe('12.35%');
    expect(fixedPct('0', 7)).toBe('0%');
    expect(fixedPct('20000000', 7)).toBe('200%');
  });
  it('is a dash for null, garbage or unknown scale, never 0', () => {
    expect(fixedPct(null, 7)).toBe('—');
    expect(fixedPct(undefined, 7)).toBe('—');
    expect(fixedPct('abc', 7)).toBe('—');
    expect(fixedPct('9500000', null)).toBe('—');
  });
});

describe('fixedDecimal and supplyCapText', () => {
  it('formats exactly and dashes nulls', () => {
    expect(fixedDecimal('10000000', 7)).toBe('1.0000000');
    expect(fixedDecimal(null, 7)).toBe('—');
    expect(supplyCapText('1000000000', 7)).toBe('100');
    expect(supplyCapText(null, 7)).toBe('—');
  });
});

describe('configKindLabel', () => {
  it('names every kind', () => {
    expect(configKindLabel('queue_set_reserve')).toBe('Reserve queued');
    expect(configKindLabel('set_reserve')).toBe('Reserve applied');
    expect(configKindLabel('cancel_set_reserve')).toBe(
      'Reserve change cancelled',
    );
    expect(configKindLabel('update_pool')).toBe('Pool updated');
    expect(configKindLabel('set_status')).toBe('Status changed');
    expect(configKindLabel('set_admin')).toBe('Admin changed');
    expect(configKindLabel('future_kind')).toBe('future_kind');
  });
});

describe('poolStatusText', () => {
  it('prefers the label, falls back to the raw code, else null', () => {
    expect(poolStatusText({ status: 1, status_label: 'active' })).toBe(
      'Active',
    );
    expect(poolStatusText({ status: 9 })).toBe('Status code 9');
    expect(poolStatusText({})).toBeNull();
    expect(poolStatusText(null)).toBeNull();
  });
});

describe('changeValues', () => {
  it('lists reserve factors and cap, with dashes for nulls', () => {
    const v = changeValues(
      {
        kind: 'set_reserve',
        config: {
          index: 0,
          decimals: 7,
          collateral_factor: '7500000',
          liability_factor: '8000000',
          target_utilization: '0',
          max_utilization: '0',
          r_base: '0',
          r_one: '0',
          r_two: '0',
          r_three: '0',
          reactivity: '0',
          supply_cap: null,
          enabled: true,
        },
      },
      7,
    );
    expect(v).toEqual([
      { label: 'Collateral factor', value: '75%' },
      { label: 'Liability factor', value: '80%' },
    ]);
  });
  it('lists only the pool fields present', () => {
    expect(
      changeValues(
        {
          kind: 'set_status',
          pool_config: { status: 3, status_label: 'on_ice' },
        },
        7,
      ),
    ).toEqual([{ label: 'Status', value: 'On ice' }]);
    expect(changeValues({ kind: 'cancel_set_reserve' }, 7)).toEqual([]);
  });
});
