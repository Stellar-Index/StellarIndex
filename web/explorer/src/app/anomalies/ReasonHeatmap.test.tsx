import { describe, it, expect } from 'vitest';
import { render, screen } from '@testing-library/react';

import { ReasonHeatmap, levelFor, servedDaysUTC } from './ReasonHeatmap';

describe('ReasonHeatmap', () => {
  it('renders one labeled row per reason with per-cell titles carrying exact counts', () => {
    const today = new Date().toISOString().slice(0, 10);
    render(
      <ReasonHeatmap
        windowDays={7}
        cells={[
          { day: today, reason: 'divergence', count: 3 },
          { day: today, reason: 'outlier_storm', count: 1 },
        ]}
      />,
    );
    // Row labels carry identity (never color-alone).
    expect(screen.getByText('divergence')).toBeInTheDocument();
    expect(screen.getByText('outlier_storm')).toBeInTheDocument();
    // Exact values live in native titles.
    expect(screen.getByTitle(`${today} · divergence: 3 freezes`)).toBeInTheDocument();
    expect(screen.getByTitle(`${today} · outlier_storm: 1 freeze`)).toBeInTheDocument();
    // The grid itself is an image with an honest description.
    expect(
      screen.getByRole('img', { name: /Freeze events per day and reason over the last 7 days/ }),
    ).toBeInTheDocument();
  });

  it('renders the empty-window message instead of a grid of fabricated zeros', () => {
    render(<ReasonHeatmap windowDays={30} cells={[]} />);
    expect(screen.getByText('No freezes in the last 30 days.')).toBeInTheDocument();
    expect(screen.queryByRole('img')).toBeNull();
  });

  it('quantizes counts onto the 4-step single-hue ramp', () => {
    expect(levelFor(0, 4)).toBe(0);
    expect(levelFor(1, 4)).toBe(0.25);
    expect(levelFor(4, 4)).toBe(1);
    // max always lands on the darkest step, even for max=1.
    expect(levelFor(1, 1)).toBe(1);
  });

  it('derives the day axis from the served data range, not the client clock', () => {
    // The axis spans min..max of the SERVED days — interior gaps are real
    // zeros and stay as columns; no columns are minted beyond the data.
    const days = servedDaysUTC(
      [{ day: '2026-07-28' }, { day: '2026-07-30' }, { day: '2026-07-28' }],
      30,
    );
    expect(days).toEqual(['2026-07-28', '2026-07-29', '2026-07-30']);
  });

  it('caps the axis at the window length ending on the newest served day', () => {
    // A single garbage far-past date must not explode the grid.
    const days = servedDaysUTC([{ day: '1970-01-01' }, { day: '2026-07-30' }], 3);
    expect(days).toEqual(['2026-07-28', '2026-07-29', '2026-07-30']);
  });

  it('ignores malformed day strings when deriving the axis', () => {
    expect(servedDaysUTC([{ day: 'not-a-day' }], 30)).toEqual([]);
  });
});
