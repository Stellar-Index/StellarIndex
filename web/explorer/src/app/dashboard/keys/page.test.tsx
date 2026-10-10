import { fireEvent, screen, waitFor } from '@testing-library/react';
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

import KeysPage from './page';

import { renderSignedInPage } from '../../../../test/dashboard-page';

afterEach(() => {
  listKeysWithLimit.mockReset();
  createKey.mockReset();
});

const renderKeysPage = () => renderSignedInPage(<KeysPage />);

function keyRow(id: string, name: string, overrides: object = {}) {
  return {
    id,
    name,
    key_prefix: `sip_${id.slice(-1).repeat(8)}`,
    tier: 'apikey',
    rate_limit_per_min: 1000,
    created_at: '2026-01-01T00:00:00Z',
    ...overrides,
  };
}

const serveKeys = (keys: object[] = []) =>
  listKeysWithLimit.mockResolvedValue({ keys, maxActiveKeys: 10 });

// Opens the form, names the key, optionally sets an expiry, and submits.
async function createNamedKey(name: string, expires?: string) {
  await waitFor(() => {
    expect(screen.getByText('New key')).toBeInTheDocument();
  });
  fireEvent.click(screen.getByText('New key'));
  fireEvent.change(screen.getByLabelText(/^Name/), { target: { value: name } });
  if (expires) {
    fireEvent.change(screen.getByLabelText(/^Expires/), {
      target: { value: expires },
    });
  }
  fireEvent.click(screen.getByText('Create key'));
}

// A scoped key must be distinguishable from a full-access one.
describe('/dashboard/keys scope display', () => {
  it('shows the confining scopes on a scoped key, and "Full access" on an unscoped one', async () => {
    serveKeys([
      keyRow('key_1', 'Read-only bot', { scopes: ['read'] }),
      keyRow('key_2', 'Full-access key'),
    ]);

    renderKeysPage();

    await waitFor(() => {
      expect(screen.getByText('Read-only bot')).toBeInTheDocument();
    });

    expect(screen.getByText('Scopes: read')).toBeInTheDocument();
    expect(screen.getByText('Full access')).toBeInTheDocument();
  });
});

// A timed-out mint may still have committed, so "Create failed" would mislead.
describe('/dashboard/keys create-key timeout', () => {
  it('shows a timeout-specific message, not the generic "Create failed"', async () => {
    serveKeys();
    createKey.mockRejectedValue(
      new DOMException('Request timed out', 'TimeoutError'),
    );

    renderKeysPage();
    await createNamedKey('prod');

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

describe('/dashboard/keys expiry', () => {
  it('sends the chosen expiry as an RFC 3339 expires_at', async () => {
    serveKeys();
    createKey.mockReturnValue(new Promise(() => {}));

    renderKeysPage();
    await createNamedKey('ci', '2099-06-15T09:30');

    await waitFor(() => expect(createKey).toHaveBeenCalledTimes(1));
    const sent = createKey.mock.calls[0][0].expires_at;
    expect(sent).toMatch(/^\d{4}-\d\d-\d\dT\d\d:\d\d:\d\dZ$/);
    expect(new Date(sent).getTime()).toBe(
      new Date(2099, 5, 15, 9, 30).getTime(),
    );
  });

  it('omits expires_at when no expiry is chosen', async () => {
    serveKeys();
    createKey.mockReturnValue(new Promise(() => {}));

    renderKeysPage();
    await createNamedKey('forever');

    await waitFor(() => expect(createKey).toHaveBeenCalledTimes(1));
    expect(createKey.mock.calls[0][0].expires_at).toBeUndefined();
  });

  it('renders an Expires column: the expiry when set, "Never" when not', async () => {
    serveKeys([
      keyRow('key_1', 'Expiring key', { expires_at: '2099-06-15T09:30:00Z' }),
      keyRow('key_2', 'Forever key'),
    ]);

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
