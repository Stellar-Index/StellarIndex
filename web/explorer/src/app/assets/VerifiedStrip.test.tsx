import { describe, it, expect } from 'vitest';
import { render, screen } from '@testing-library/react';

import { VerifiedStrip } from './VerifiedStrip';

describe('VerifiedStrip', () => {
  it('links each on-chain verified currency and leaves fiat out', () => {
    render(
      <VerifiedStrip
        items={[
          {
            ticker: 'USDC',
            slug: 'usdc',
            name: 'USD Coin',
            class: 'stablecoin',
            verified_issuer: 'centre.io',
          },
          { ticker: 'EUR', slug: 'euro', name: 'Euro', class: 'fiat' },
        ]}
      />,
    );
    const usdc = screen.getByRole('link', { name: /USDC/ });
    expect(usdc).toHaveAttribute('href', '/assets/usdc');
    expect(usdc).toHaveAttribute('title', 'USD Coin — verified by centre.io');
    expect(screen.queryByText('EUR')).toBeNull();
  });

  it('renders nothing when the catalogue is empty', () => {
    const { container } = render(<VerifiedStrip items={[]} />);
    expect(container.textContent).toBe('');
  });
});
