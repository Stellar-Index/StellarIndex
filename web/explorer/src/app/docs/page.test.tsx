import { describe, it, expect } from 'vitest';
import { render, screen } from '@testing-library/react';

import DocsPage from './page';

// The versioning section must describe the semver contract without claiming
// v1 has shipped/stabilized, consistent with the pre-v1 status stated on
// /company, /careers and /contact.
describe('DocsPage versioning copy', () => {
  it('does not claim "v1 is stable"', () => {
    render(<DocsPage />);
    expect(screen.queryByText(/v1 is stable/i)).not.toBeInTheDocument();
  });

  it('describes the semver contract and honest pre-v1 status', () => {
    render(<DocsPage />);
    expect(screen.getByText(/follows\s+semver/i)).toBeInTheDocument();
    expect(screen.getByText(/pre-v1/i)).toBeInTheDocument();
  });
});

describe('DocsPage rate-limit copy', () => {
  it('publishes both real limits', () => {
    render(<DocsPage />);
    const para = screen.getByText(/Anonymous reads are limited per source IP/i);
    expect(para).toHaveTextContent(/1,000\s*\n?\s*req\/min/i);
    expect(para).toHaveTextContent(/6,000\s*\n?\s*req\/min/i);
  });
});
