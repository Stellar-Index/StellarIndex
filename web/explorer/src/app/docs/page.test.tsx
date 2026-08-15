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
