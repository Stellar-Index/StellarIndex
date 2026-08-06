import { describe, it, expect } from 'vitest';
import { render, screen } from '@testing-library/react';

import { DirectoryLabel } from './DirectoryLabel';

describe('DirectoryLabel', () => {
  it('renders the curated name, tags, and source attribution', () => {
    render(
      <DirectoryLabel
        info={{
          name: 'SDF Growth 3',
          domain: 'stellar.org',
          tags: ['sdf', 'custodian'],
          source: 'stellar-expert',
        }}
      />,
    );
    expect(screen.getByText('SDF Growth 3')).toBeInTheDocument();
    expect(screen.getByText('#sdf')).toBeInTheDocument();
    expect(screen.getByText('#custodian')).toBeInTheDocument();
    // Attribution must always be present — these are third-party
    // labels, never our own verification claim.
    expect(
      screen.getByRole('link', { name: /StellarExpert public directory/i }),
    ).toHaveAttribute('href', 'https://github.com/stellar-expert/public-directory');
    expect(screen.getByText(/listing is not endorsement/i)).toBeInTheDocument();
  });

  it('warns prominently on malicious/unsafe tags', () => {
    render(
      <DirectoryLabel
        info={{ name: 'Fake Wallet', tags: ['malicious'], source: 'stellar-expert' }}
      />,
    );
    expect(screen.getByText(/Flagged malicious/i)).toBeInTheDocument();
    expect(screen.getByText(/treat with caution/i)).toBeInTheDocument();
  });

  it('renders no warning line for benign tags', () => {
    render(
      <DirectoryLabel
        info={{ name: 'Binance Hot', tags: ['exchange'], source: 'stellar-expert' }}
      />,
    );
    expect(screen.queryByText(/treat with caution/i)).not.toBeInTheDocument();
  });
});
