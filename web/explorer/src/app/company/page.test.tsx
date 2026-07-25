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
});
