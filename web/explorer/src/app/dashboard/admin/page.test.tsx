import { screen } from '@testing-library/react';
import { describe, expect, it, vi } from 'vitest';

vi.mock('@/api/hooks', async () => {
  const actual =
    await vi.importActual<typeof import('@/api/hooks')>('@/api/hooks');
  return { ...actual, useMe: vi.fn() };
});

vi.mock('next/navigation', async () => {
  const actual =
    await vi.importActual<typeof import('next/navigation')>('next/navigation');
  return {
    ...actual,
    useRouter: () => ({ ...actual.useRouter(), replace: vi.fn() }),
  };
});

import { renderSignedInPage } from '../../../../test/dashboard-page';
import AdminPage from './page';

// The staff cockpit and the customer look-up form are gated on
// me.user.is_staff, not merely on being signed in — a non-staff customer
// hitting /dashboard/admin must see the restricted-area callout, never
// the look-up tool.
describe('/dashboard/admin staff gate', () => {
  it.each([
    ['a non-staff user', false, 'Restricted area', false],
    ['a staff user', true, 'Staff cockpit', true],
  ])('renders the right view for %s', (_who, isStaff, heading, hasLookup) => {
    renderSignedInPage(<AdminPage />, {
      user: { email: 'a@b.com', is_staff: isStaff },
    });

    expect(screen.getByText(heading)).toBeInTheDocument();
    const lookup = screen.queryByLabelText('Customer email or account slug');
    if (hasLookup) {
      expect(lookup).toBeInTheDocument();
    } else {
      expect(lookup).not.toBeInTheDocument();
    }
  });
});
