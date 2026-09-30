import { describe, expect, it } from 'vitest';

import { trailingEnvelope } from './envelope';

const H = 3600;
const T0 = Date.parse('2026-07-03T00:00:00Z') / 1000;

function bar(hour: number, h: string, l: string) {
  return { t: new Date((T0 + hour * H) * 1000).toISOString(), h, l };
}

describe('trailingEnvelope', () => {
  it('takes the trailing high/low over a 4h window of 1h bars', () => {
    const bars = [
      bar(0, '1.0', '0.9'),
      bar(1, '1.2', '0.95'),
      bar(2, '1.1', '0.8'),
      bar(3, '1.05', '1.0'),
      bar(4, '1.01', '0.99'),
    ];
    expect(trailingEnvelope(bars, 4 * H, H)).toEqual([
      { time: T0 + 3 * H, upper: '1.2', lower: '0.8' },
      { time: T0 + 4 * H, upper: '1.2', lower: '0.8' },
    ]);
  });

  it('drops bars that fall out of the window', () => {
    const bars = [bar(0, '9', '0.1'), bar(1, '2', '1.5'), bar(2, '3', '1.4')];
    expect(trailingEnvelope(bars, 2 * H, H)).toEqual([
      { time: T0 + H, upper: '9', lower: '0.1' },
      { time: T0 + 2 * H, upper: '3', lower: '1.4' },
    ]);
  });

  it('spans a gap with only the bars actually served', () => {
    // No bars at hours 2–4 (no trades); the 4h window at hour 5 holds 5 alone.
    const bars = [
      bar(0, '5', '4'),
      bar(1, '6', '3'),
      bar(5, '2', '1.9'),
      bar(6, '2.5', '2'),
    ];
    expect(trailingEnvelope(bars, 4 * H, H)).toEqual([
      { time: T0 + 5 * H, upper: '2', lower: '1.9' },
      { time: T0 + 6 * H, upper: '2.5', lower: '1.9' },
    ]);
  });

  it('compares exactly where doubles collide', () => {
    // Both highs round to the same double; only exact comparison picks the larger.
    const bars = [
      bar(0, '0.10000000000000000001', '0.1'),
      bar(1, '0.10000000000000000002', '0.1'),
    ];
    expect(trailingEnvelope(bars, 2 * H, H)[0].upper).toBe(
      '0.10000000000000000002',
    );
  });

  it('omits a point whose window holds a non-decimal value', () => {
    const bars = [bar(0, '1', '0.5'), bar(1, 'NaN', '0.4'), bar(2, '1', '0.6')];
    expect(trailingEnvelope(bars, 2 * H, H)).toEqual([]);
    expect(trailingEnvelope(bars, H, H)).toEqual([
      { time: T0, upper: '1', lower: '0.5' },
      { time: T0 + 2 * H, upper: '1', lower: '0.6' },
    ]);
  });
});
