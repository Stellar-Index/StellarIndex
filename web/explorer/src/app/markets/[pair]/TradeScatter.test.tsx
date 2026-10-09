import { describe, expect, it } from 'vitest';
import { render } from '@testing-library/react';

import { TradeScatter, scatterDots } from './TradeScatter';

const T = (
  ts: string,
  price: string,
  base_amount: string,
  source = 'sdex',
) => ({
  ts,
  source,
  price,
  base_amount,
});

describe('scatterDots', () => {
  it('maps the oldest-cheapest trade bottom-left and sizes by base amount', () => {
    const s = scatterDots([
      T('2026-10-09T00:00:00Z', '0.1', '10000000'),
      T('2026-10-09T01:00:00Z', '0.2', '40000000', 'soroswap'),
    ]);
    expect(s?.sources).toEqual(['sdex', 'soroswap']);
    const [a, b] = s!.dots;
    expect(a.x).toBeLessThan(b.x);
    expect(a.y).toBeGreaterThan(b.y);
    expect(b.r).toBe(9);
    expect(a.r).toBeCloseTo(2 + 0.5 * 7);
  });

  it('keeps the exact price and amount strings in the tooltip', () => {
    const s = scatterDots([
      T('2026-10-09T00:00:00Z', '0.1234567', '123456789012345678901'),
      T('2026-10-09T01:00:00Z', '0.2', '1'),
    ]);
    expect(s!.dots[0].title).toContain('0.1234567');
    expect(s!.dots[0].title).toContain('12345678901234.5678901');
  });

  it('needs two plottable trades', () => {
    expect(
      scatterDots([T('2026-10-09T00:00:00Z', '0.1', '1'), T('bad', 'x', '1')]),
    ).toBeNull();
  });
});

describe('TradeScatter', () => {
  it('renders one dot per trade', () => {
    const { container } = render(
      <TradeScatter
        trades={[
          T('2026-10-09T00:00:00Z', '0.1', '1'),
          T('2026-10-09T01:00:00Z', '0.2', '2'),
        ]}
      />,
    );
    expect(container.querySelectorAll('circle')).toHaveLength(2);
  });
});
