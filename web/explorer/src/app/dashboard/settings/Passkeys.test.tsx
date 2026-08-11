import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { fireEvent, render, screen, waitFor } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';

import type { PasskeyCredential } from '@/api/account';

import { Passkeys } from './Passkeys';

const listPasskeys = vi.hoisted(() => vi.fn());
const deletePasskey = vi.hoisted(() => vi.fn());
vi.mock('@/api/account', async (importOriginal) => ({
  ...(await importOriginal<typeof import('@/api/account')>()),
  listPasskeys,
  deletePasskey,
  beginPasskeyRegister: vi.fn(),
  finishPasskeyRegister: vi.fn(),
}));

afterEach(() => {
  listPasskeys.mockReset();
  deletePasskey.mockReset();
});

function passkey(overrides: Partial<PasskeyCredential> = {}): PasskeyCredential {
  return {
    id: 'a1b2c3d4-0000-4000-8000-000000000001',
    name: 'MacBook Touch ID',
    transports: ['internal'],
    backup_eligible: true,
    created_at: '2026-08-01T12:00:00Z',
    last_used_at: '2026-08-10T12:00:00Z',
    ...overrides,
  };
}

function renderPasskeys() {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(
    <QueryClientProvider client={qc}>
      <Passkeys />
    </QueryClientProvider>,
  );
}

describe('Passkeys settings section', () => {
  it('lists registered passkeys with their metadata', async () => {
    listPasskeys.mockResolvedValue([
      passkey(),
      passkey({
        id: 'a1b2c3d4-0000-4000-8000-000000000002',
        name: 'YubiKey',
        backup_eligible: false,
        last_used_at: undefined,
      }),
    ]);

    renderPasskeys();

    expect(await screen.findByText('MacBook Touch ID')).toBeInTheDocument();
    expect(screen.getByText('YubiKey')).toBeInTheDocument();
    // Synced badge only on the backup-eligible credential.
    expect(screen.getAllByText('synced')).toHaveLength(1);
    // Never-used credential says so.
    expect(screen.getByText(/never used/)).toBeInTheDocument();
    // Each row has an accessible remove control.
    expect(
      screen.getByRole('button', { name: /Remove passkey YubiKey/ }),
    ).toBeInTheDocument();
  });

  it('shows the empty state when no passkeys are registered', async () => {
    listPasskeys.mockResolvedValue([]);
    renderPasskeys();
    expect(await screen.findByText('No passkeys yet')).toBeInTheDocument();
  });

  it('removes a passkey via the DELETE endpoint and refreshes the list', async () => {
    listPasskeys.mockResolvedValue([passkey()]);
    deletePasskey.mockResolvedValue(undefined);

    renderPasskeys();
    fireEvent.click(
      await screen.findByRole('button', { name: /Remove passkey MacBook Touch ID/ }),
    );

    await waitFor(() =>
      expect(deletePasskey).toHaveBeenCalledWith(
        'a1b2c3d4-0000-4000-8000-000000000001',
      ),
    );
    // The list is re-fetched after a successful delete.
    await waitFor(() => expect(listPasskeys.mock.calls.length).toBeGreaterThan(1));
  });

  it('tells an unsupported browser that passkeys are unavailable (jsdom has no WebAuthn)', async () => {
    listPasskeys.mockResolvedValue([]);
    renderPasskeys();
    expect(
      await screen.findByText(/doesn.t support passkeys/),
    ).toBeInTheDocument();
  });
});
