import { describe, it, expect, vi } from 'vitest';
import { fireEvent, render, screen } from '@testing-library/react';

vi.mock('@/api/hooks', () => ({
  useIssuers: () => ({
    data: [
      {
        g_strkey: 'GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN',
        org_name: 'Alpha',
        org_verified: true,
        home_domain: 'alpha.example',
        asset_count: 3,
        total_observation_count: 4000,
      },
      {
        g_strkey: 'GDUKMGUGDZQK6YHYA5Z6AY2G4XDSZPSZ3SW5UN3ARVMO6QSRDWP5YLEX',
        org_name: 'Beta',
        home_domain: 'beta.example/@evil',
        asset_count: 1,
        total_observation_count: 1000,
      },
    ],
    isLoading: false,
    isError: false,
    error: null,
    refetch: () => undefined,
  }),
}));

import { IssuersTable } from './IssuersTable';

describe('IssuersTable', () => {
  it('bars each issuer’s observations against the busiest in view', () => {
    render(<IssuersTable />);
    const bar = (n: string) =>
      screen.getByRole('img', { name: `${n} observations` })
        .firstElementChild as HTMLElement;
    expect(bar('4,000').style.width).toBe('100%');
    expect(bar('1,000').style.width).toBe('25%');
  });

  it('badges the bidirectionally verified issuer only', () => {
    render(<IssuersTable />);
    expect(screen.getAllByText('Verified')).toHaveLength(1);
  });

  it('links only a plain-hostname home domain', () => {
    render(<IssuersTable />);
    expect(screen.getByText('alpha.example').closest('a')).toHaveAttribute(
      'href',
      'https://alpha.example',
    );
    expect(screen.getByText('beta.example/@evil').closest('a')).toBeNull();
  });

  it('keeps the directory rank when the view is filtered', () => {
    render(<IssuersTable />);
    fireEvent.change(
      screen.getByLabelText('Filter issuers by name, domain, or account'),
      { target: { value: 'beta' } },
    );
    const rows = screen.getAllByRole('row');
    expect(rows).toHaveLength(2); // header + Beta
    expect(rows[1].querySelector('td')?.textContent).toBe('2');
  });
});
