import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { fireEvent, render, screen } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { SESSION_HINT_COOKIE } from '@/api/sessionHint';

const replace = vi.hoisted(() => vi.fn());
vi.mock('next/navigation', async () => {
  const actual =
    await vi.importActual<typeof import('next/navigation')>('next/navigation');
  return {
    ...actual,
    useRouter: () => ({ ...actual.useRouter(), replace }),
  };
});

import SettingsPage from './page';

const ME = {
  data: {
    user: { email: 'a@b.com', role: 'owner' },
    account: { name: 'Brimstone Labs', slug: 'brimstone-labs', tier: 'pro' },
  },
};

let deleteStatus = 204;
let deleteBody: string | null = null;

beforeEach(() => {
  deleteStatus = 204;
  deleteBody = null;
  document.cookie = `${SESSION_HINT_COOKIE}=1; Path=/`;
  vi.spyOn(globalThis, 'fetch').mockImplementation(async (input, init) => {
    const url = String(input);
    if (url.includes('/v1/account/me')) {
      return new Response(JSON.stringify(ME), { status: 200 });
    }
    if (url.includes('/v1/dashboard/account') && init?.method === 'DELETE') {
      deleteBody = String(init.body);
      return new Response(
        deleteStatus === 204 ? null : JSON.stringify({ detail: 'nope' }),
        { status: deleteStatus },
      );
    }
    return new Response(JSON.stringify({ data: [] }), { status: 200 });
  });
});

afterEach(() => {
  document.cookie = `${SESSION_HINT_COOKIE}=; Max-Age=0; Path=/`;
  replace.mockReset();
  vi.restoreAllMocks();
});

function renderPage() {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  render(
    <QueryClientProvider client={qc}>
      <SettingsPage />
    </QueryClientProvider>,
  );
}

describe('/dashboard/settings account deletion', () => {
  it('requires the typed slug, then deletes and leaves the dashboard', async () => {
    renderPage();
    fireEvent.click(
      await screen.findByRole('button', { name: 'Delete account' }),
    );
    const confirmBtn = screen.getByRole('button', {
      name: /Delete account permanently/,
    });
    expect(confirmBtn).toBeDisabled();
    fireEvent.change(screen.getByLabelText(/Type the account slug/), {
      target: { value: 'brimstone-labs' },
    });
    fireEvent.click(confirmBtn);
    await vi.waitFor(() => expect(replace).toHaveBeenCalledWith('/'));
    expect(JSON.parse(deleteBody ?? '{}')).toEqual({
      confirm: 'brimstone-labs',
    });
  });

  it('a 401 (session too old) bounces to sign-in, not to the home page', async () => {
    deleteStatus = 401;
    renderPage();
    fireEvent.click(
      await screen.findByRole('button', { name: 'Delete account' }),
    );
    fireEvent.change(screen.getByLabelText(/Type the account slug/), {
      target: { value: 'brimstone-labs' },
    });
    fireEvent.click(
      screen.getByRole('button', { name: /Delete account permanently/ }),
    );
    await vi.waitFor(() => expect(replace).toHaveBeenCalledWith('/signin'));
    expect(replace).not.toHaveBeenCalledWith('/');
  });

  it('shows the blocked message on 409', async () => {
    deleteStatus = 409;
    renderPage();
    fireEvent.click(
      await screen.findByRole('button', { name: 'Delete account' }),
    );
    fireEvent.change(screen.getByLabelText(/Type the account slug/), {
      target: { value: 'brimstone-labs' },
    });
    fireEvent.click(
      screen.getByRole('button', { name: /Delete account permanently/ }),
    );
    expect(await screen.findByText(/handled by support/)).toBeInTheDocument();
    expect(replace).not.toHaveBeenCalled();
  });
});
