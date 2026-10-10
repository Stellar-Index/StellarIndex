import { render, screen } from '@testing-library/react';
import { describe, expect, it } from 'vitest';
import { PositionAmount } from './AccountDefiPositions';

describe('PositionAmount', () => {
  it('scales a known-token amount and keeps the exact value in the title', () => {
    render(<PositionAmount amount="90071992547409930000000" decimals={7} />);
    expect(screen.getByTitle('9007199254740993').textContent).toBe('9007.2T');
  });

  it('serves an unscaled amount raw when no scale is known', () => {
    render(<PositionAmount amount="1234567" />);
    expect(screen.getByText('1234567')).toBeTruthy();
  });

  it('renders an unknown amount as a dash, never zero', () => {
    render(<PositionAmount amount="" decimals={7} />);
    expect(screen.getByText('—')).toBeTruthy();
  });
});
