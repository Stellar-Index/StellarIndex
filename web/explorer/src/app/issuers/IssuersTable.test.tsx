import { describe, it, expect, vi } from 'vitest';
import { render, screen } from '@testing-library/react';

vi.mock('@/api/hooks', () => ({
  useIssuers: () => ({
    data: [
      {
        g_strkey: 'GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN',
        org_name: 'Alpha',
        asset_count: 3,
        total_observation_count: 4000,
      },
      {
        g_strkey: 'GDUKMGUGDZQK6YHYA5Z6AY2G4XDSZPSZ3SW5UN3ARVMO6QSRDWP5YLEX',
        org_name: 'Beta',
        asset_count: 1,
        total_observation_count: 1000,
      },
    ],
    isLoading: false,
    isError: false,
    error: null,
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
});
