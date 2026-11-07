import { act } from 'react';
import { hydrateRoot, type Root } from 'react-dom/client';
import { renderToString } from 'react-dom/server';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

const { replace, network } = vi.hoisted(() => ({
  replace: vi.fn(),
  network: { accounts: true },
}));

vi.mock('next/navigation', async () => {
  const actual =
    await vi.importActual<typeof import('next/navigation')>('next/navigation');
  return { ...actual, useRouter: () => ({ replace }) };
});

vi.mock('@/lib/networks', async () => {
  const actual =
    await vi.importActual<typeof import('@/lib/networks')>('@/lib/networks');
  return {
    ...actual,
    CURRENT_NETWORK: {
      ...actual.CURRENT_NETWORK,
      get accounts() {
        return network.accounts;
      },
    },
  };
});

import { SESSION_HINT_COOKIE } from '@/api/sessionHint';
import { AccountGate } from './AccountGate';

// The explorer is a static export, so useSessionHint reads false for the
// hydration render even when the cookie is there. Only a real hydrate sees that.
function hydrateGate(): { container: HTMLElement; root: Root } {
  const tree = () => (
    <QueryClientProvider
      client={
        new QueryClient({ defaultOptions: { queries: { retry: false } } })
      }
    >
      <AccountGate>{() => <div>dashboard content</div>}</AccountGate>
    </QueryClientProvider>
  );
  const container = document.createElement('div');
  document.body.appendChild(container);
  container.innerHTML = renderToString(tree());
  let root!: Root;
  act(() => {
    root = hydrateRoot(container, tree());
  });
  return { container, root };
}

function stubMe(status: number, body: unknown) {
  vi.stubGlobal(
    'fetch',
    vi.fn(async () => new Response(JSON.stringify(body), { status })),
  );
}

describe('AccountGate hydrating over a static shell', () => {
  let mounted: Root | undefined;

  beforeEach(() => {
    replace.mockClear();
    network.accounts = true;
    document.cookie = `${SESSION_HINT_COOKIE}=1; Path=/`;
  });

  afterEach(() => {
    act(() => mounted?.unmount());
    mounted = undefined;
    document.body.innerHTML = '';
    document.cookie = `${SESSION_HINT_COOKIE}=; Max-Age=0; Path=/`;
    vi.unstubAllGlobals();
  });

  it('keeps a signed-in visitor on the dashboard', async () => {
    stubMe(200, { data: { user: { email: 'a@b.com' } } });
    const { container, root } = hydrateGate();
    mounted = root;

    await vi.waitFor(() =>
      expect(container.textContent).toContain('dashboard content'),
    );
    expect(replace).not.toHaveBeenCalled();
  });

  it('redirects when the hint has outlived the session', async () => {
    stubMe(401, {});
    const { root } = hydrateGate();
    mounted = root;

    await vi.waitFor(() => expect(replace).toHaveBeenCalledWith('/signin'));
  });

  it('redirects on a network with no accounts backend, despite a shared hint', async () => {
    network.accounts = false;
    stubMe(503, {});
    const { root } = hydrateGate();
    mounted = root;

    await vi.waitFor(() => expect(replace).toHaveBeenCalledWith('/signin'));
  });
});
