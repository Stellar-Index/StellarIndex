import { render, screen } from '@testing-library/react';
import { describe, expect, it } from 'vitest';

import { LiquidityVenueBars, buildVenueRows } from './LiquidityVenueBars';

describe('LiquidityVenueBars', () => {
  it('sums pools per venue and ranks venues', () => {
    render(
      <LiquidityVenueBars
        pools={[
          { source: 'soroswap', volume_24h_usd: '10.50' },
          { source: 'soroswap', volume_24h_usd: '20.25' },
          { source: 'phoenix', volume_24h_usd: '100' },
        ]}
      />,
    );
    const rows = screen.getAllByRole('listitem');
    expect(rows[0]).toHaveTextContent('phoenix');
    expect(rows[1]).toHaveTextContent('$30');
    expect(rows[1]).toHaveTextContent('2 pools');
  });

  it('marks lower-bound venues and keeps >2^53 sums exact', () => {
    const rows = buildVenueRows([
      {
        source: 'sdex',
        volume_24h_usd: '9007199254740993',
        volume_lower_bound: true,
      },
      { source: 'sdex', volume_24h_usd: '2' },
    ]);
    expect(rows[0].display).toBe('≥ $9,007,199,254,740,995');
    expect(rows[0].hatchTail).toBe(true);
  });

  it('renders nothing for fewer than two venues', () => {
    const { container } = render(
      <LiquidityVenueBars pools={[{ source: 'x', volume_24h_usd: '5' }]} />,
    );
    expect(container.firstChild).toBeNull();
  });
  it('marks a venue with an unvalued pool as a lower bound', () => {
    const [row] = buildVenueRows([
      { source: 'amm', volume_24h_usd: '100' },
      { source: 'amm', volume_24h_usd: null },
    ]);
    expect(row?.display).toBe('≥ $100');
    expect(row?.hatchTail).toBe(true);
  });
});
