import { describe, it, expect } from 'vitest';
import { render, screen } from '@testing-library/react';

import CompanyPage from './page';

// "v1 ships in the coming weeks" reads as a false promise while the
// product is pre-v1 (see CHANGELOG.md, still on 0.x releases). The page must not assert a specific ship timeframe it
// cannot back up.
describe('CompanyPage', () => {
  it('does not promise a stale "coming weeks" v1 ship date', () => {
    render(<CompanyPage />);
    expect(screen.queryByText(/coming weeks/i)).not.toBeInTheDocument();
    // still communicates honest pre-v1 status without a fabricated date
    expect(screen.getByText(/still pre-v1/i)).toBeInTheDocument();
  });

  // The public roadmap link must point at the maintained plan, not a
  // retired tracker.
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

  // The "honest about what we don't have" bullet pointed readers at
  // /methodology for "every gap and the path to closing each".
  // /methodology has eight sections — sources, VWAP, stablecoin proxy,
  // freeze, closed-bucket, latency, precision, audit — and no gap list
  // anywhere. The gaps are enumerated in this bullet itself, so the
  // link must describe what /methodology actually carries.
  it('does not claim /methodology lists every gap', () => {
    render(<CompanyPage />);
    expect(screen.queryByText(/lists every gap/i)).not.toBeInTheDocument();
    expect(
      screen.getByText(/documents how each number is computed/i),
    ).toBeInTheDocument();
  });
});
