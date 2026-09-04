import { describe, it, expect } from 'vitest';
import { render, screen } from '@testing-library/react';

import MethodologyPage from './page';

// The closed-bucket section described "Three regions ingest
// independently" in the present tense. One region serves the API:
// /v1/status reports `region.name = r1`, and /sla, /company and
// /careers all say one region on the same site. ADR-0050's
// three-region active/active topology is ratified and scheduled after
// v1.0 — the invariant is what makes it safe, not a description of
// today's deployment.
describe('MethodologyPage closed-bucket section', () => {
  it('does not claim three regions ingest today', () => {
    render(<MethodologyPage />);
    expect(
      screen.queryByText(/Three regions ingest independently/i),
    ).not.toBeInTheDocument();
  });

  it('states the single live region and dates the multi-region plan', () => {
    render(<MethodologyPage />);
    const para = screen.getByText(/load-bearing invariant behind/i);
    expect(para).toHaveTextContent(/One region serves the API today/i);
    expect(para).toHaveTextContent(/ADR-0050/);
    expect(para).toHaveTextContent(/after v1\.0/i);
  });
});
