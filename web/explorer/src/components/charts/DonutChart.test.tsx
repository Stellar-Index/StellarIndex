import { describe, it, expect } from 'vitest';
import { render, screen } from '@testing-library/react';

import { DonutChart } from './DonutChart';
import { ratioPct, sumDecimalStrings } from '@/lib/format';

describe('DonutChart exact allocation percentage', () => {
  it('agrees with ratioPct (the table math) for decimal inputs above 2^53', () => {
    // Neither is exactly representable as a double (ADR-0003) — a float
    // legend % would silently diverge from what a table cell computes
    // via ratioPct for the same wire values.
    const a = '9007199254740993'; // 2^53 + 1
    const b = '12345678901234567';
    const total = sumDecimalStrings([a, b])!;
    const expectedPctA = ratioPct(a, total, 1)!;
    const expectedPctB = ratioPct(b, total, 1)!;

    render(
      <DonutChart
        data={[
          { label: 'Alpha', value: Number(a), decimal: a },
          { label: 'Beta', value: Number(b), decimal: b },
        ]}
      />,
    );

    const label = screen.getByRole('img').getAttribute('aria-label') ?? '';
    expect(label).toContain(`Alpha ${expectedPctA.toFixed(1)}%`);
    expect(label).toContain(`Beta ${expectedPctB.toFixed(1)}%`);
  });

  it('falls back to the float ratio when any slice omits the decimal string', () => {
    render(
      <DonutChart
        data={[
          { label: 'Alpha', value: 3 },
          { label: 'Beta', value: 1 },
        ]}
      />,
    );
    const label = screen.getByRole('img').getAttribute('aria-label') ?? '';
    expect(label).toContain('Alpha 75.0%');
    expect(label).toContain('Beta 25.0%');
  });
});
