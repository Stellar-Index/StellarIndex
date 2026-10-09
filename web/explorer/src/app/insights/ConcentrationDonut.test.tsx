import { describe, it, expect } from 'vitest';
import { render, screen } from '@testing-library/react';

import { ConcentrationDonut } from './ConcentrationDonut';

const rows = [
  { id: 'a', label: 'AAA', count: 60 },
  { id: 'b', label: 'BBB', count: 20 },
  { id: 'c', label: 'CCC', count: 5 },
];

describe('ConcentrationDonut', () => {
  it('computes the top share against the served total', () => {
    render(
      <ConcentrationDonut
        rows={rows}
        total={200}
        noun="creators"
        what="accounts created"
        topN={2}
      />,
    );
    expect(screen.getByTestId('concentration-donut').textContent).toContain(
      'Top 2 creators hold 40% of all accounts created.',
    );
  });

  it('is exact for counts beyond float-safe division noise', () => {
    render(
      <ConcentrationDonut
        rows={[
          { id: 'a', label: 'A', count: 1 },
          { id: 'b', label: 'B', count: 2 },
        ]}
        total={3}
        noun="x"
        what="y"
      />,
    );
    expect(screen.getByTestId('concentration-donut').textContent).toContain(
      'hold 100%',
    );
  });

  it('renders nothing for a single entity', () => {
    const { container } = render(
      <ConcentrationDonut
        rows={rows.slice(0, 1)}
        total={100}
        noun="a"
        what="b"
      />,
    );
    expect(container.firstChild).toBeNull();
  });
});
