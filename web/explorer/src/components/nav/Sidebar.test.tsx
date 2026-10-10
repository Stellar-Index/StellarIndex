import { describe, it, expect, vi, beforeEach } from 'vitest';
import {
  render,
  screen,
  fireEvent,
  waitFor,
  within,
} from '@testing-library/react';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';

// Hermetic: the Status pill fetches /v1/status on mount. restoreMocks
// un-spies between tests; the pill cases re-spy with their own resolutions.
beforeEach(() => {
  vi.spyOn(globalThis, 'fetch').mockRejectedValue(
    new TypeError('no network in tests'),
  );
});

vi.mock('@/api/hooks', async () => {
  const actual =
    await vi.importActual<typeof import('@/api/hooks')>('@/api/hooks');
  return {
    ...actual,
    useMe: () => ({
      data: { user: { email: 'signed-in@example.com', is_staff: false } },
      isLoading: false,
      isError: false,
    }),
  };
});

import { SidebarNav } from './Sidebar';
import { ConsoleShell } from './ConsoleShell';

vi.mock('next/navigation', () => ({
  usePathname: () => '/',
  useRouter: () => ({ push: vi.fn() }),
}));

function withClient(ui: React.ReactElement) {
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  });
  return render(
    <QueryClientProvider client={client}>{ui}</QueryClientProvider>,
  );
}

function renderNav() {
  withClient(<SidebarNav />);
}

describe('Sidebar IA', () => {
  it('renders the three sections with their entries and the Stellar wordmark', () => {
    renderNav();
    // Section headers ("Stellar" also appears inside the split wordmark,
    // so match on the header styling class, not bare text).
    for (const title of ['Stellar', 'External', 'Developers']) {
      const headers = screen
        .getAllByText(title)
        .filter((el) => el.className.includes('uppercase'));
      expect(headers).toHaveLength(1);
    }
    // Wordmark — split spans ("Stellar" + lighter "Index") whose
    // accessible name still reads StellarIndex.
    expect(
      screen.getByRole('link', { name: /Stellar\s*Index/ }),
    ).toHaveAttribute('href', '/');
    const hrefs = [
      ['Network', '/network'],
      ['Transactions', '/transactions'],
      ['Accounts', '/accounts'],
      ['Contracts', '/contracts'],
      ['SDEX', '/sdex'],
      ['Protocols', '/protocols'],
      ['Oracles', '/oracles'],
      ['Insights', '/insights'],
      // External: Markets is the CEX board.
      ['Markets', '/exchanges'],
      ['SDK', '/sdk'],
    ] as const;
    for (const [label, href] of hrefs) {
      expect(screen.getByRole('link', { name: label })).toHaveAttribute(
        'href',
        href,
      );
    }
    // Two "Assets" links exist (Stellar + External).
    const assetLinks = screen.getAllByRole('link', { name: 'Assets' });
    expect(assetLinks.map((a) => a.getAttribute('href')).sort()).toEqual([
      '/assets',
      '/external/assets',
    ]);
    expect(screen.getByRole('link', { name: /API Docs/ })).toHaveAttribute(
      'href',
      'https://docs.stellarindex.io',
    );
    // The accessible name carries the tone dot's sr-only suffix; match the prefix.
    expect(screen.getByRole('link', { name: /^Status/ })).toHaveAttribute(
      'href',
      '/status',
    );
    // Retired rail entries must NOT come back silently.
    for (const gone of [
      'AMM Pools',
      'External Markets',
      'Verification',
      'Home',
    ]) {
      expect(
        screen.queryByRole('link', { name: gone }),
      ).not.toBeInTheDocument();
    }
  });
});

describe('Sidebar Status pill', () => {
  it('reflects the live /v1/status overall state on the Status row', async () => {
    vi.spyOn(globalThis, 'fetch').mockResolvedValue({
      ok: true,
      status: 200,
      json: async () => ({ data: { overall: 'degraded' } }),
    } as Response);
    renderNav();
    expect(
      await screen.findByText('(degraded performance)'),
    ).toBeInTheDocument();
  });

  it('makes NO claim when the status feed cannot be reached', async () => {
    renderNav();
    expect(await screen.findByText('(status unknown)')).toBeInTheDocument();
    expect(
      screen.queryByText('(all systems operational)'),
    ).not.toBeInTheDocument();
  });
});

describe('Sidebar AccountMenu', () => {
  it('restores focus to the trigger button after closing with Escape, even when focus had moved into the panel', () => {
    renderNav();
    const trigger = screen.getByRole('button', {
      name: /signed-in@example.com/,
    });
    trigger.focus();
    fireEvent.click(trigger);

    // Focus moves into the open panel first; otherwise the check is vacuous.
    const accountLink = screen.getByRole('link', { name: /Your account/ });
    accountLink.focus();
    expect(document.activeElement).toBe(accountLink);

    fireEvent.keyDown(document, { key: 'Escape' });

    expect(screen.queryByText('Sign out')).not.toBeInTheDocument();
    // Not <body>, where an unmounted focused element's focus falls by default.
    expect(document.activeElement).toBe(trigger);
  });

  it.each([
    ['signs out on an empty-body 200 and navigates home', 200, '/'],
    ['stays put and shows an error when logout fails', 500, '/dashboard'],
  ])('%s', async (_name, status, finalHref) => {
    const loc = { ...window.location, href: '/dashboard' };
    Object.defineProperty(window, 'location', {
      configurable: true,
      value: loc,
    });
    vi.spyOn(globalThis, 'fetch').mockImplementation(
      async () => new Response(null, { status }),
    );
    renderNav();
    fireEvent.click(
      screen.getByRole('button', { name: /signed-in@example.com/ }),
    );
    fireEvent.click(screen.getByRole('button', { name: 'Sign out' }));
    if (status === 200) {
      await waitFor(() => expect(loc.href).toBe(finalHref));
    } else {
      expect(await screen.findByRole('alert')).toHaveTextContent(/failed/i);
      expect(loc.href).toBe(finalHref);
    }
    vi.unstubAllGlobals();
  });
});

// The rail and the mobile drawer each mount a SearchModal; Cmd-K must never
// leave two "Site search" dialogs stacked.
describe('ConsoleShell search dialogs', () => {
  function renderShell() {
    withClient(
      <ConsoleShell>
        <div />
      </ConsoleShell>,
    );
    fireEvent.click(screen.getByRole('button', { name: 'Open navigation' }));
  }
  const dialogs = () => screen.getAllByRole('dialog', { name: 'Site search' });

  it('opens one dialog on Cmd-K while the drawer is open', () => {
    renderShell();
    fireEvent.keyDown(window, { key: 'k', metaKey: true });
    expect(dialogs()).toHaveLength(1);
  });

  it('keeps one dialog when Cmd-K follows the drawer search button', () => {
    renderShell();
    const drawer = screen.getByRole('dialog', { name: 'Navigation' });
    fireEvent.click(
      within(drawer).getByRole('button', { name: 'Open search' }),
    );
    expect(dialogs()).toHaveLength(1);
    fireEvent.keyDown(window, { key: 'k', metaKey: true });
    expect(dialogs()).toHaveLength(1);
  });
});
