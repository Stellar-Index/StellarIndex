import { describe, it, expect } from 'vitest';
import { render, screen } from '@testing-library/react';

import { PoolDepthDetail, type PoolDepthRow } from './PoolDepthDetail';

const ROW: PoolDepthRow = {
  pool: 'LBM7UHOFOQZ5Z66S3NZRTUTMWPNB6KHS3AEVUPWDNLKO7HFNWAAT5OI6',
  fee_bps: 30,
  as_of_ledger: 63734668,
  reserve_a: { asset: 'native', decimals: 7, reserve: '5450058925055' },
  reserve_b: {
    asset: 'AQUA-GBNZILSTVQZ4R7IKQDGHYGY2QXL5QOFJYQMXPKWRRM5PAV7Y4M67AQUA',
    decimals: 7,
    reserve: '2764706604547318',
  },
  mid_price_b_in_a: '0.001971297394121634',
  depth: [
    {
      slippage_pct: '0.5',
      asset_a_in: { max_input: '10987855879', output: '5546051363029' },
      asset_b_in: { max_input: '5573920968024', output: '10932916599' },
    },
    {
      slippage_pct: '1',
      asset_a_in: { max_input: '38651725353', output: '19411179771036' },
      asset_b_in: { max_input: '19607252294085', output: '38265208099' },
    },
  ],
};

describe('PoolDepthDetail', () => {
  it('renders per-direction slippage bars + the mid-price-valued reserve donut', () => {
    render(<PoolDepthDetail row={ROW} />);
    // Both directions, each its own chart (different units — never one axis).
    expect(screen.getByText('Sell XLM → get AQUA')).toBeInTheDocument();
    expect(screen.getByText('Sell AQUA → get XLM')).toBeInTheDocument();
    // Tier labels render per direction.
    expect(screen.getAllByText('≤ 0.5%')).toHaveLength(2);
    // Exact fill amounts stay on the bar labels (1098.7855879 XLM ≈ 1.1K).
    expect(screen.getByText('1.1K XLM')).toBeInTheDocument();
    // The donut + its constant-product caveat.
    expect(
      screen.getByText(/Reserve composition — valued at the pool/),
    ).toBeInTheDocument();
    expect(screen.getByText(/≈50\/50 by value/)).toBeInTheDocument();
    // The model caveat survives the table→chart swap.
    expect(screen.getByText(/not\s+an order book/)).toBeInTheDocument();
  });

  it('renders the empty-side message instead of charts when depth is absent', () => {
    render(<PoolDepthDetail row={{ ...ROW, depth: [] }} />);
    expect(
      screen.getByText(/One side of this pool is empty/),
    ).toBeInTheDocument();
  });
});
