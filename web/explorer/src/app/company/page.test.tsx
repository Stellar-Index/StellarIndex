import { describe, it, expect } from 'vitest';
import { render, screen } from '@testing-library/react';

import CompanyPage from './page';

// LC-001: "v1 ships in the coming weeks" was hardcoded on 2026-05-07 and
// never updated; by 2026-07-24 the product is still pre-v1 (see
// CHANGELOG.md, still on 0.x releases), so the stale timeline reads as a
// false promise. The page must not assert a specific ship timeframe it
// cannot back up.
describe('CompanyPage', () => {
  it('does not promise a stale "coming weeks" v1 ship date', () => {
    render(<CompanyPage />);
    expect(screen.queryByText(/coming weeks/i)).not.toBeInTheDocument();
    // still communicates honest pre-v1 status without a fabricated date
    expect(screen.getByText(/still pre-v1/i)).toBeInTheDocument();
  });

  // #321: the page told the public that "the roadmap that gets us to v1"
  // lives in launch-readiness-backlog.md — a doc frozen since 2026-05-13
  // that contains none of the actual v1 gate, and is now formally retired.
  // The public roadmap link must point at the maintained plan.
  it('points the public roadmap link at the maintained launch plan, not the retired backlog', () => {
    render(<CompanyPage />);

    const roadmap = screen.getByRole('link', { name: /v1-launch-plan\.md/i });
    expect(roadmap).toHaveAttribute(
      'href',
      'https://github.com/Stellar-Index/StellarIndex/blob/main/docs/operations/v1-launch-plan.md',
    );

    // The retired backlog must not be linked anywhere on the page.
    const links = screen.getAllByRole('link');
    for (const link of links) {
      expect(link.getAttribute('href') ?? '').not.toContain(
        'launch-readiness-backlog',
      );
    }
  });
});
