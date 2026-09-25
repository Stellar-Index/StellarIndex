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

const listKeysWithLimit = vi.hoisted(() => vi.fn());
vi.mock('@/api/account', async (importOriginal) => ({
  ...(await importOriginal<typeof import('@/api/account')>()),
  listKeysWithLimit,
}));

import { useMe } from '@/api/hooks';
import KeysPage from './page';

afterEach(() => {
  listKeysWithLimit.mockReset();
});

function renderKeysPage() {
  vi.mocked(useMe).mockReturnValue({
    isLoading: false,
    isError: false,
    data: { user: { email: 'a@b.com' } },
    refetch: vi.fn(),
  } as unknown as ReturnType<typeof useMe>);

  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(
    <QueryClientProvider client={qc}>
      <KeysPage />
    </QueryClientProvider>,
  );
}

// A key confined to a scope subset must render as such — otherwise it is
// indistinguishable from a full-access key at the render level, and a
// customer auditing their own keys has no way to tell a `read`-only key
// from one that can mint webhooks or revoke other keys.
describe('/dashboard/keys scope display', () => {
  it('shows the confining scopes on a scoped key, and "Full access" on an unscoped one', async () => {
    listKeysWithLimit.mockResolvedValue({
      keys: [
        {
          id: 'key_1',
          name: 'Read-only bot',
          key_prefix: 'sip_aaaaaaaa',
          tier: 'apikey',
          rate_limit_per_min: 1000,
          scopes: ['read'],
          created_at: '2026-01-01T00:00:00Z',
        },
        {
          id: 'key_2',
          name: 'Full-access key',
          key_prefix: 'sip_bbbbbbbb',
          tier: 'apikey',
          rate_limit_per_min: 1000,
          created_at: '2026-01-01T00:00:00Z',
        },
      ],
      maxActiveKeys: 10,
    });

    renderKeysPage();

    await waitFor(() => {
      expect(screen.getByText('Read-only bot')).toBeInTheDocument();
    });

    expect(screen.getByText('Scopes: read')).toBeInTheDocument();
    expect(screen.getByText('Full access')).toBeInTheDocument();
  });
});
