import { describe, expect, it } from 'vitest';
import { render, screen } from '@testing-library/react';

import { SurvivalDonut, survivalSlices } from './SurvivalDonut';

describe('survivalSlices', () => {
  it('splits a cohort into live and gone', () => {
    expect(survivalSlices(1200, 900)?.map((s) => [s.id, s.value])).toEqual([
      ['live', 900],
      ['gone', 300],
    ]);
  });

  it.each([
    ['an empty cohort', 0, 0],
    ['more live than members', 10, 11],
    ['a negative live count', 10, -1],
    ['a fractional count', 10.5, 3],
  ])('returns null for %s', (_, accounts, live) => {
    expect(survivalSlices(accounts, live)).toBeNull();
  });
});

describe('SurvivalDonut', () => {
  it('centres the exact live share', () => {
    render(<SurvivalDonut accounts={1200} live={900} />);
    expect(screen.getByTestId('survival-donut').textContent).toMatch(
      /75(\.0)?%/,
    );
  });
});
