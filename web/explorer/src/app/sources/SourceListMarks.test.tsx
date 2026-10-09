import { describe, it, expect } from 'vitest';
import { render, screen } from '@testing-library/react';

import { CountBar, LagDot, lagTone } from './SourceListMarks';

describe('SourceListMarks', () => {
  it('buckets lag at 60s and 600s', () => {
    expect([30, 60, 599, 600].map(lagTone)).toEqual([
      'ok',
      'warn',
      'warn',
      'bad',
    ]);
  });

  it('renders a lag dot with its tone', () => {
    render(<LagDot lagSeconds={900} />);
    expect(screen.getByRole('img')).toHaveAttribute('data-tone', 'bad');
  });

  it('scales the bar against the group max and hides for zero', () => {
    const { container, rerender } = render(<CountBar value={50} max={200} />);
    expect(
      (container.firstElementChild!.firstElementChild as HTMLElement).style
        .width,
    ).toBe('25%');
    rerender(<CountBar value={0} max={200} />);
    expect(container.firstChild).toBeNull();
  });
});
