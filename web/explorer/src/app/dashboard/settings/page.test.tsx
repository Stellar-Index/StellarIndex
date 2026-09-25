import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { render, screen, waitFor } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';

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

const listPasskeys = vi.hoisted(() => vi.fn());
vi.mock('@/api/account', async (importOriginal) => ({
  ...(await importOriginal<typeof import('@/api/account')>()),
  listPasskeys,
}));

import { useMe } from '@/api/hooks';
import SettingsPage from './page';

afterEach(() => {
  listPasskeys.mockReset();
});

function renderSettingsPage() {
  vi.mocked(useMe).mockReturnValue({
    isLoading: false,
    isError: false,
    data: {
      user: { email: 'a@b.com', display_name: 'Ash', role: 'owner' },
      account: { name: 'Brimstone Labs', slug: 'brimstone-labs', tier: 'pro' },
    },
    refetch: vi.fn(),
  } as unknown as ReturnType<typeof useMe>);

  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(
    <QueryClientProvider client={qc}>
      <SettingsPage />
    </QueryClientProvider>,
  );
}

describe('/dashboard/settings', () => {
  it('renders the profile, plan and danger zone sections', async () => {
    listPasskeys.mockResolvedValue([]);

    renderSettingsPage();

    expect(screen.getByText('a@b.com')).toBeInTheDocument();
    expect(screen.getByText('Danger zone')).toBeInTheDocument();
    expect(screen.getByRole('button', { name: /Sign out/ })).toBeInTheDocument();

    await waitFor(() => {
      expect(screen.getByText('No passkeys yet')).toBeInTheDocument();
    });
  });
});
