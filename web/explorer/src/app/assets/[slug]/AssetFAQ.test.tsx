import { describe, it, expect } from 'vitest';
import { render, screen } from '@testing-library/react';

import { AssetFAQ } from './AssetFAQ';

describe('AssetFAQ', () => {
  it('renders the FAQ panel with the asset code interpolated', () => {
    render(<AssetFAQ symbol="XLM" hasIssuer={false} />);
    expect(screen.getByText('FAQ')).toBeInTheDocument();
    expect(screen.getByText('What is XLM?')).toBeInTheDocument();
    // no-issuer branch → Soroban/contract-token phrasing
    expect(screen.getByText(/Soroban-native or smart-contract token/)).toBeInTheDocument();
  });

  it('uses classic-issuer phrasing when hasIssuer is set', () => {
    render(<AssetFAQ symbol="USDC" hasIssuer />);
    expect(screen.getByText('USDC issuer details')).toBeInTheDocument();
    // phrasing unique to the has-issuer branch of assetFaqFor
    expect(
      screen.getByText(/As a classic credit asset, USDC has a designated issuer account/),
    ).toBeInTheDocument();
  });
});
