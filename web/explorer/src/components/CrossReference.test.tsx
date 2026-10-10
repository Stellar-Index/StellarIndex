import { render, screen } from '@testing-library/react';
import { describe, expect, it } from 'vitest';

import { CrossReference } from './CrossReference';

const hrefs = () =>
  screen.getAllByRole('link').map((a) => a.getAttribute('href'));

describe('CrossReference', () => {
  it('links an account to all three explorers', () => {
    render(<CrossReference kind="account" id="GABC" />);
    expect(screen.getByText(/External references/)).toBeTruthy();
    expect(hrefs()).toEqual([
      'https://stellar.expert/explorer/public/account/GABC',
      'https://stellarchain.io/accounts/GABC',
      'https://steexp.com/account/GABC',
    ]);
  });

  it('omits stellarchain.io where it has no verified route', () => {
    render(<CrossReference kind="ledger" id="123" />);
    expect(hrefs()).toEqual([
      'https://stellar.expert/explorer/public/ledger/123',
      'https://steexp.com/ledger/123',
    ]);
  });
});
