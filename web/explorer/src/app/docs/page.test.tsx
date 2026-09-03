import { describe, it, expect } from 'vitest';
import { render, screen } from '@testing-library/react';

import DocsPage from './page';

// UXP-11 (F3): the docs page asserted "v1 is stable" while /company,
// /careers, and /contact all state the product is still pre-v1 /
// pre-launch — opposite stability signals for a data product whose
// prices drive integrations. The versioning section must describe the
// semver contract without falsely claiming v1 has shipped/stabilized,
// staying consistent with the pre-v1 status stated elsewhere.
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

// The quickstart said an API key "raises your limits". On the hosted
// deployment it lowers them: the anonymous header is
// `x-ratelimit-limit: 6000` and a free key is stamped at 1,000. That is
// deliberate and /pricing explains it — the docs have to say the same
// thing, or an integrator mints a key expecting headroom and gets a
// sixfold cut.
describe('DocsPage rate-limit copy', () => {
  it('does not claim a key raises your limits', () => {
    render(<DocsPage />);
    expect(screen.queryByText(/raises your limits/i)).not.toBeInTheDocument();
  });

  it('publishes both real limits and says a key is a budget, not a raise', () => {
    render(<DocsPage />);
    const para = screen.getByText(/Anonymous reads are limited per source IP/i);
    expect(para).toHaveTextContent(/6,000\s*\n?\s*req\/min/i);
    expect(para).toHaveTextContent(/1,000\s*\n?\s*req\/min/i);
    expect(para).toHaveTextContent(/not extra throughput/i);
  });
});
