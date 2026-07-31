import { describe, it, expect } from 'vitest';

import { unwrapChangeSummary, buildChangeWindows } from './ChangeSummaryStrip';
import type { ChangeSummary } from '@/api/hooks';

const row: ChangeSummary = {
  entity_type: 'coin',
  entity_id: 'crypto:XLM',
  refreshed_at: '2026-07-31T14:07:22Z',
  current_value: '0.168',
  h1_delta_pct: -1.02,
  h24_delta_pct: -2.46,
  d7_delta_pct: -6.42,
  // d30 absent — sparse-history rows really omit windows (verified live
  // 2026-07-31: /v1/changes/coin/native served h1/h24/d7 but no d30).
  atl_value: '0.1399',
  atl_at: '2026-05-23T08:41:00Z',
  streak_direction: 'down',
  streak_days: 1,
  acceleration: 'decreasing',
};

describe('unwrapChangeSummary', () => {
  it('unwraps the live envelope shape ({data, as_of, flags})', () => {
    const out = unwrapChangeSummary({ data: row, as_of: 'x', flags: {} });
    expect(out?.entity_id).toBe('crypto:XLM');
  });

  it('accepts an already-bare row (future-proof against the hook unwrapping)', () => {
    expect(unwrapChangeSummary(row)?.entity_id).toBe('crypto:XLM');
  });

  it('returns null for garbage instead of a fabricated row', () => {
    expect(unwrapChangeSummary(undefined)).toBeNull();
    expect(unwrapChangeSummary(null)).toBeNull();
    expect(unwrapChangeSummary('nope')).toBeNull();
    expect(unwrapChangeSummary({})).toBeNull();
  });
});

describe('buildChangeWindows', () => {
  it('builds the canonical 1h/24h/7d/30d strip, keeping absent windows null', () => {
    const windows = buildChangeWindows(row);
    expect(windows.map((w) => w.label)).toEqual(['1h', '24h', '7d', '30d']);
    expect(windows[0].deltaPct).toBeCloseTo(-1.02);
    expect(windows[3].deltaPct).toBeNull(); // absent ≠ zero
  });
});
