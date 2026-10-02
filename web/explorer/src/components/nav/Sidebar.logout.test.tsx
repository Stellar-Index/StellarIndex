import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { render, screen, fireEvent } from '@testing-library/react';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';

import { SESSION_HINT_COOKIE } from '@/api/sessionHint';
import { SidebarNav } from './Sidebar';

// Uses the real useMe: a failed logout must not flip the visitor to
// signed-out, or the menu (and its error) unmounts.
const ME = {
  data: { user: { email: 'signed-in@example.com', is_staff: false } },
};

beforeEach(() => {
  document.cookie = `${SESSION_HINT_COOKIE}=1; Path=/`;
  vi.spyOn(globalThis, 'fetch').mockImplementation(async (input) => {
    const url = String(input);
    if (url.includes('/v1/account/me')) {
      return new Response(JSON.stringify(ME), { status: 200 });
    }
    if (url.includes('/v1/auth/logout')) {
      return new Response(null, { status: 500 });
    }
    throw new TypeError('no network in tests');
  });
});

afterEach(() => {
  document.cookie = `${SESSION_HINT_COOKIE}=; Max-Age=0; Path=/`;
});

describe('Sidebar sign-out failure with the real useMe', () => {
  it('keeps the menu, shows the error and does not navigate', async () => {
    const loc = { ...window.location, href: '/dashboard' };
    Object.defineProperty(window, 'location', {
      configurable: true,
      value: loc,
    });
    const client = new QueryClient({
      defaultOptions: { queries: { retry: false } },
    });
    render(
      <QueryClientProvider client={client}>
        <SidebarNav />
      </QueryClientProvider>,
    );
    fireEvent.click(
      await screen.findByRole('button', { name: /signed-in@example.com/ }),
    );
    fireEvent.click(screen.getByRole('button', { name: 'Sign out' }));
    expect(await screen.findByRole('alert')).toHaveTextContent(/failed/i);
    expect(loc.href).toBe('/dashboard');
    expect(
      screen.getByRole('button', { name: /signed-in@example.com/ }),
    ).toBeInTheDocument();
  });
});
