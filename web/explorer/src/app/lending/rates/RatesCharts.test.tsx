import { describe, it, expect } from 'vitest';
import { fireEvent, render, screen } from '@testing-library/react';

import { RatesCharts, assetAprItems, scatterPoints } from './RatesCharts';
import type { LendingRate } from './LendingRatesTable';

const row = (over: Partial<LendingRate>): LendingRate =>
  ({
    protocol: 'blend',
    pool: 'CAJJZSGMMM3PD7N33TAPHGBUGTB43OC73HVIK2L2G6BNGGGYOSSYBXBD',
    asset: 'CUSDC',
    asset_id: 'native',
    supply_apr: 0.05,
    borrow_apr: 0.08,
    utilization_pct: 62.5,
    supplied_usd: '1234567.891',
    borrowed_usd: '1',
    obtainable: true,
    ...over,
  }) as LendingRate;

describe('scatterPoints', () => {
  it('maps utilisation, APR and supplied USD, keeping the exact USD for the tooltip', () => {
    const { points } = scatterPoints([row({})], 'supply');
    expect(points[0]).toMatchObject({ x: 62.5, y: 5, size: 1234567.891 });
    expect(points[0].lines.join(' ')).toContain('$1,234,567.89');
    expect(points[0].lines[0]).toBe('XLM · Pool #1 (genesis)');
    expect(scatterPoints([row({})], 'borrow').points[0].y).toBeCloseTo(8);
  });

  it('omits unpriced and unrated reserves and counts them; never a zero-size dot', () => {
    const r = scatterPoints(
      [
        row({}),
        row({ pool: 'P2', supplied_usd: null }),
        row({ pool: 'P3', borrow_apr: null }),
      ],
      'borrow',
    );
    expect(r.points).toHaveLength(1);
    expect(r.unpriced).toBe(1);
    expect(r.unrated).toBe(1);
  });

  it('draws a non-obtainable rate hollow', () => {
    expect(
      scatterPoints([row({ obtainable: false })], 'supply').points[0].hollow,
    ).toBe(true);
  });
});

describe('RatesCharts', () => {
  it('toggles the y metric and shows the per-pool bar only under an asset filter', () => {
    const rows = [
      row({}),
      row({ pool: 'P2', supply_apr: 0.09, protocol: 'blnt' }),
    ];
    const { rerender } = render(<RatesCharts rows={rows} asset="" />);
    expect(screen.getAllByTestId('scatter-dot')).toHaveLength(2);
    expect(screen.queryByText(/by pool/)).toBeNull();
    fireEvent.click(screen.getByRole('button', { name: 'Borrow APR' }));
    expect(
      screen.getByRole('group', { name: /Borrow APR against utilisation/ }),
    ).toBeInTheDocument();
    rerender(<RatesCharts rows={rows} asset="native" />);
    expect(screen.getByText(/Supply APR for .* by pool/)).toBeInTheDocument();
    expect(assetAprItems(rows).map((i) => i.display)).toEqual([
      '9.00%',
      '5.00%',
    ]);
  });

  it('has an empty state', () => {
    render(<RatesCharts rows={[row({ supplied_usd: null })]} asset="" />);
    expect(screen.getByText(/No priced reserve/)).toBeInTheDocument();
    expect(screen.getByText(/1 not priced/)).toBeInTheDocument();
  });
});
