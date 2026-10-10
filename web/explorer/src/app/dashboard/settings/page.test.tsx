import { fireEvent, screen, waitFor } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';

vi.mock('@/api/hooks', async () => {
  const actual =
    await vi.importActual<typeof import('@/api/hooks')>('@/api/hooks');
  return { ...actual, useMe: vi.fn() };
});

const replace = vi.hoisted(() => vi.fn());
vi.mock('next/navigation', async () => {
  const actual =
    await vi.importActual<typeof import('next/navigation')>('next/navigation');
  return {
    ...actual,
    useRouter: () => ({ ...actual.useRouter(), replace }),
  };
});

const listPasskeys = vi.hoisted(() => vi.fn());
vi.mock('@/api/account', async (importOriginal) => ({
  ...(await importOriginal<typeof import('@/api/account')>()),
  listPasskeys,
}));

import { renderSignedInPage } from '../../../../test/dashboard-page';
import SettingsPage from './page';

afterEach(() => {
  vi.unstubAllGlobals();
  replace.mockReset();
  listPasskeys.mockReset();
});

const ME = {
  user: { email: 'a@b.com', display_name: 'Ash', role: 'owner' },
  account: { name: 'Brimstone Labs', slug: 'brimstone-labs', tier: 'pro' },
};

const renderSettingsPage = () => renderSignedInPage(<SettingsPage />, ME);

describe('/dashboard/settings', () => {
  it('renders the profile, plan and danger zone sections', async () => {
    listPasskeys.mockResolvedValue([]);

    renderSettingsPage();

    expect(screen.getByText('a@b.com')).toBeInTheDocument();
    expect(screen.getByText('Danger zone')).toBeInTheDocument();
    expect(
      screen.getByRole('button', { name: /Sign out/ }),
    ).toBeInTheDocument();

    await waitFor(() => {
      expect(screen.getByText('No passkeys yet')).toBeInTheDocument();
    });
  });

  it('signs out on an empty-body 200 and bounces to /signin', async () => {
    listPasskeys.mockResolvedValue([]);
    vi.stubGlobal(
      'fetch',
      vi.fn().mockResolvedValue(new Response(null, { status: 200 })),
    );
    renderSettingsPage();
    fireEvent.click(screen.getByRole('button', { name: /Sign out/ }));
    await waitFor(() => expect(replace).toHaveBeenCalledWith('/signin'));
  });

  it('does not bounce and shows an error when logout fails', async () => {
    listPasskeys.mockResolvedValue([]);
    vi.stubGlobal(
      'fetch',
      vi.fn().mockResolvedValue(new Response(null, { status: 500 })),
    );
    renderSettingsPage();
    fireEvent.click(screen.getByRole('button', { name: /Sign out/ }));
    expect(await screen.findByText(/Sign out failed/)).toBeInTheDocument();
    expect(replace).not.toHaveBeenCalled();
  });
});
