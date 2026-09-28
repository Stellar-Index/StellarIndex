import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { fireEvent, render, screen, waitFor } from '@testing-library/react';
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
const createKey = vi.hoisted(() => vi.fn());
vi.mock('@/api/account', async (importOriginal) => ({
  ...(await importOriginal<typeof import('@/api/account')>()),
  listKeysWithLimit,
  createKey,
}));

import { useMe } from '@/api/hooks';
import KeysPage from './page';

afterEach(() => {
  listKeysWithLimit.mockReset();
  createKey.mockReset();
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

// GH-1076: a mint that times out client-side may still have committed
// server-side, so the bare "Create failed" fallback is misleading — it
// must name the timeout and point the customer at the key list.
describe('/dashboard/keys create-key timeout', () => {
  it('shows a timeout-specific message, not the generic "Create failed"', async () => {
    listKeysWithLimit.mockResolvedValue({ keys: [], maxActiveKeys: 10 });
    createKey.mockRejectedValue(
      new DOMException('Request timed out', 'TimeoutError'),
    );

    renderKeysPage();

    await waitFor(() => {
      expect(screen.getByText('New key')).toBeInTheDocument();
    });
    fireEvent.click(screen.getByText('New key'));

    fireEvent.change(screen.getByLabelText(/^Name/), {
      target: { value: 'prod' },
    });
    fireEvent.click(screen.getByText('Create key'));

    await waitFor(() => {
      expect(
        screen.getByText(
          'The request timed out — check your key list before retrying.',
        ),
      ).toBeInTheDocument();
    });
    expect(screen.queryByText('Create failed')).not.toBeInTheDocument();
  });
});

// GH-1073: the server accepts and enforces expires_at, but the dashboard
// offered no way to set it and no way to see it.
describe('/dashboard/keys expiry', () => {
  it('sends the chosen expiry as an RFC 3339 expires_at', async () => {
    listKeysWithLimit.mockResolvedValue({ keys: [], maxActiveKeys: 10 });
    createKey.mockReturnValue(new Promise(() => {}));

    renderKeysPage();

    await waitFor(() => {
      expect(screen.getByText('New key')).toBeInTheDocument();
    });
    fireEvent.click(screen.getByText('New key'));
    fireEvent.change(screen.getByLabelText(/^Name/), {
      target: { value: 'ci' },
    });
    fireEvent.change(screen.getByLabelText(/^Expires/), {
      target: { value: '2099-06-15T09:30' },
    });
    fireEvent.click(screen.getByText('Create key'));

    await waitFor(() => expect(createKey).toHaveBeenCalledTimes(1));
    const sent = createKey.mock.calls[0][0].expires_at;
    expect(sent).toMatch(/^\d{4}-\d\d-\d\dT\d\d:\d\d:\d\dZ$/);
    expect(new Date(sent).getTime()).toBe(
      new Date(2099, 5, 15, 9, 30).getTime(),
    );
  });

  it('omits expires_at when no expiry is chosen', async () => {
    listKeysWithLimit.mockResolvedValue({ keys: [], maxActiveKeys: 10 });
    createKey.mockReturnValue(new Promise(() => {}));

    renderKeysPage();

    await waitFor(() => {
      expect(screen.getByText('New key')).toBeInTheDocument();
    });
    fireEvent.click(screen.getByText('New key'));
    fireEvent.change(screen.getByLabelText(/^Name/), {
      target: { value: 'forever' },
    });
    fireEvent.click(screen.getByText('Create key'));

    await waitFor(() => expect(createKey).toHaveBeenCalledTimes(1));
    expect(createKey.mock.calls[0][0].expires_at).toBeUndefined();
  });

  it('renders an Expires column: the expiry when set, "Never" when not', async () => {
    listKeysWithLimit.mockResolvedValue({
      keys: [
        {
          id: 'key_1',
          name: 'Expiring key',
          key_prefix: 'sip_aaaaaaaa',
          tier: 'apikey',
          rate_limit_per_min: 1000,
          created_at: '2026-01-01T00:00:00Z',
          expires_at: '2099-06-15T09:30:00Z',
        },
        {
          id: 'key_2',
          name: 'Forever key',
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
      expect(screen.getByText('Expiring key')).toBeInTheDocument();
    });
    expect(
      screen.getByRole('columnheader', { name: 'Expires' }),
    ).toBeInTheDocument();
    expect(screen.getByTitle('2099-06-15T09:30:00Z')).toHaveTextContent(/2099/);
    expect(screen.getByText('Never')).toBeInTheDocument();
  });
});
