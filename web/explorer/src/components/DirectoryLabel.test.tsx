import { describe, it, expect } from 'vitest';
import { render, screen } from '@testing-library/react';

import { DIRECTORY_SCAM_FLAG_TAGS } from '@/lib/directory-tags';

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
    ).toHaveAttribute(
      'href',
      'https://github.com/stellar-expert/public-directory',
    );
    expect(screen.getByText(/listing is not endorsement/i)).toBeInTheDocument();
  });

  it('warns prominently on malicious/unsafe tags', () => {
    render(
      <DirectoryLabel
        info={{
          name: 'Fake Wallet',
          tags: ['malicious'],
          source: 'stellar-expert',
        }}
      />,
    );
    expect(screen.getByText(/Flagged malicious/i)).toBeInTheDocument();
    expect(screen.getByText(/treat with caution/i)).toBeInTheDocument();
  });

  it('renders no warning line for benign tags', () => {
    render(
      <DirectoryLabel
        info={{
          name: 'Binance Hot',
          tags: ['exchange'],
          source: 'stellar-expert',
        }}
      />,
    );
    expect(screen.queryByText(/treat with caution/i)).not.toBeInTheDocument();
    expect(screen.getByText('#exchange')).toHaveClass('text-ink-body');
  });

  // The gate has to be the CANONICAL scam vocabulary, not a copy of two
  // of its six members. DIRECTORY_SCAM_FLAG_TAGS is the one frontend
  // list — pinned equal to the Go DirectoryScamFlagTags by pricingguard's
  // TestScamFlagTagSet_MatchesFrontend — and every tag in it makes the
  // server WITHHOLD the issuer's price and rank its assets last. An
  // address the API refuses to price must not render on /accounts/{G…}
  // or /contract as a neutral grey label. Enumerated FROM the exported
  // list rather than retyped, so this fails the day a component
  // re-introduces a private subset, and covers a seventh tag on the day
  // it is added.
  it.each([...DIRECTORY_SCAM_FLAG_TAGS])(
    'warns on the #%s directory tag',
    (tag) => {
      render(
        <DirectoryLabel
          info={{ name: 'Fake Wallet', tags: [tag], source: 'stellar-expert' }}
        />,
      );
      expect(
        screen.getByText(new RegExp(`Flagged ${tag} by`, 'i')),
      ).toBeInTheDocument();
      expect(screen.getByText(/treat with caution/i)).toBeInTheDocument();
      // The pill itself carries the danger tone, not only the sentence.
      expect(screen.getByText(`#${tag}`)).toHaveClass('text-bad-700');
    },
  );

  // Tags reach the browser verbatim: directory-sync stores upstream's
  // JSON strings unchanged and dirInfoV copies them unfiltered, so the
  // served spelling is whatever the public directory shipped. Matching is
  // case-insensitive server-side (pricingguard.IsDirectoryScamFlagged)
  // and must be here too, or #Phishing withholds the price while the page
  // says nothing.
  it('warns on a non-lowercase spelling, and leaves benign tags neutral', () => {
    render(
      <DirectoryLabel
        info={{
          name: 'Fake Wallet',
          tags: ['exchange', 'Phishing'],
          source: 'stellar-expert',
        }}
      />,
    );
    expect(screen.getByText(/Flagged Phishing by/)).toBeInTheDocument();
    expect(screen.getByText(/treat with caution/i)).toBeInTheDocument();
    expect(screen.getByText('#Phishing')).toHaveClass('text-bad-700');
    // The benign sibling keeps the neutral tone and is not named as a flag.
    expect(screen.getByText('#exchange')).toHaveClass('text-ink-body');
    expect(screen.queryByText(/Flagged exchange/)).not.toBeInTheDocument();
    // Attribution survives the warning path.
    expect(screen.getByText(/listing is not endorsement/i)).toBeInTheDocument();
  });
});
