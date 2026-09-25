import { render, screen } from '@testing-library/react';
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

import { useMe } from '@/api/hooks';
import AdminPage from './page';

function mockMe(isStaff: boolean) {
  vi.mocked(useMe).mockReturnValue({
    isLoading: false,
    isError: false,
    data: { user: { email: 'a@b.com', is_staff: isStaff } },
    refetch: vi.fn(),
  } as unknown as ReturnType<typeof useMe>);
}

// The staff cockpit and the customer look-up form are gated on
// me.user.is_staff, not merely on being signed in — a non-staff customer
// hitting /dashboard/admin must see the restricted-area callout, never
// the look-up tool.
describe('/dashboard/admin staff gate', () => {
  it('shows a restricted-area notice for a non-staff user', () => {
    mockMe(false);

    render(<AdminPage />);

    expect(screen.getByText('Restricted area')).toBeInTheDocument();
    expect(
      screen.queryByLabelText('Customer email or account slug'),
    ).not.toBeInTheDocument();
  });

  it('shows the staff cockpit with the customer look-up form for a staff user', () => {
    mockMe(true);

    render(<AdminPage />);

    expect(screen.getByText('Staff cockpit')).toBeInTheDocument();
    expect(
      screen.getByLabelText('Customer email or account slug'),
    ).toBeInTheDocument();
  });
});
