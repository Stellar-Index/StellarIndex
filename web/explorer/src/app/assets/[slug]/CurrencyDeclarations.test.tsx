import { describe, it, expect } from 'vitest';
import { render, screen } from '@testing-library/react';

import { type AssetDetail } from '@/api/hooks';
import { CurrencyDeclarations } from './CurrencyDeclarations';

const base = {
  asset_id: 'BOND-GAAA',
  sep1_status: 'verified',
} as unknown as AssetDetail;

describe('CurrencyDeclarations', () => {
  it('renders each declared field and links the reserve attestation', () => {
    render(
      <CurrencyDeclarations
        asset={{
          ...base,
          currency_status: 'live',
          is_asset_anchored: true,
          attestation_of_reserve: 'https://issuer.example.com/reserves.pdf',
          redemption_instructions: 'Redeem through the portal.',
          regulated: false,
        }}
      />,
    );
    expect(screen.getByText('live')).toBeInTheDocument();
    expect(screen.getByText('is_asset_anchored')).toBeInTheDocument();
    expect(screen.getByText('Redeem through the portal.')).toBeInTheDocument();
    expect(
      screen.getByRole('link', {
        name: 'https://issuer.example.com/reserves.pdf',
      }),
    ).toHaveAttribute('href', 'https://issuer.example.com/reserves.pdf');
    // A declared false is a claim and is shown; it is not "not declared".
    expect(screen.getByText('regulated')).toBeInTheDocument();
    expect(screen.getByText('false')).toBeInTheDocument();
  });

  it('renders nothing when the issuer declared none of the fields', () => {
    const { container } = render(<CurrencyDeclarations asset={base} />);
    expect(container).toBeEmptyDOMElement();
  });
});
