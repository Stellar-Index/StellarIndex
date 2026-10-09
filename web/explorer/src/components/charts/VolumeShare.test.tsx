import { describe, it, expect } from 'vitest';
import { render, screen } from '@testing-library/react';

import { VolumeShare, topWithOther } from './VolumeShare';

const rows = [
  { id: 'a', label: 'A', volume: '9007199254740993' },
  { id: 'b', label: 'B', volume: '5' },
  { id: 'c', label: 'C', volume: '3' },
  { id: 'd', label: 'D', volume: null },
];

describe('VolumeShare', () => {
  it('sums the tail exactly into other, above 2^53', () => {
    const r = topWithOther(rows, 1);
    expect(r.top.map((x) => x.id)).toEqual(['a']);
    expect(r.other).toBe('8');
    expect(r.total).toBe('9007199254741001');
    expect(r.pricedCount).toBe(3);
  });

  it('renders bars, donut and the top-N-of-M note', () => {
    render(<VolumeShare rows={rows} topN={2} noun="pairs" />);
    expect(screen.getByLabelText('Top 2 pairs by 24h USD volume')).toBeTruthy();
    expect(screen.getByTestId('volume-share').textContent).toContain(
      'Top 2 of 3 pairs',
    );
  });

  it('renders nothing without priced volume', () => {
    const { container } = render(<VolumeShare rows={[rows[3]]} noun="pairs" />);
    expect(container.firstChild).toBeNull();
  });
});
