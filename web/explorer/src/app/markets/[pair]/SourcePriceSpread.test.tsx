import { describe, it, expect } from 'vitest';
import { render, screen } from '@testing-library/react';

import { SourcePriceSpread } from './SourcePriceSpread';

describe('SourcePriceSpread', () => {
  it('shows the exact spread between lowest and highest venue', () => {
    render(
      <SourcePriceSpread
        rows={[
          { source: 'soroswap', last_price: '0.10' },
          { source: 'sdex', last_price: '0.1025' },
          { source: 'phoenix', last_price: null },
        ]}
      />,
    );
    expect(screen.getByText('2.50%')).toBeTruthy();
    expect(screen.getByLabelText(/2 venues, spread 2.50%/)).toBeTruthy();
  });

  it('renders nothing with fewer than two priced venues', () => {
    const { container } = render(
      <SourcePriceSpread rows={[{ source: 'sdex', last_price: '1' }]} />,
    );
    expect(container.firstChild).toBeNull();
  });
});
