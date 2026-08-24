import { describe, it, expect, vi, beforeEach } from 'vitest';
import { render, screen, fireEvent } from '@testing-library/react';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';

// Hermetic by default: the Status pill's shared useStatus query fetches
// /v1/status on mount — never let a component test reach the network.
// (config `restoreMocks: true` un-spies between tests; the pill cases
// re-spy with their own resolutions.)
beforeEach(() => {
  vi.spyOn(globalThis, 'fetch').mockRejectedValue(
    new TypeError('no network in tests'),
  );
});

vi.mock('@/api/hooks', async () => {
  const actual = await vi.importActual<typeof import('@/api/hooks')>('@/api/hooks');
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

// ACC-06: the account menu had no focus-trap/focus-restore — closing it
// (Escape) never returned focus to the trigger button, unlike the shared
// useDialog hook already used elsewhere (RequestReveal).
function renderNav() {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  render(
    <QueryClientProvider client={client}>
      <SidebarNav />
    </QueryClientProvider>,
  );
}

describe('Sidebar IA (nav revision 2026-08-24)', () => {
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
    expect(screen.getByRole('link', { name: /Stellar\s*Index/ })).toHaveAttribute('href', '/');
    // Stellar section entries, in spec order
    for (const [label, href] of [
      ['Network', '/network'],
      ['Transactions', '/transactions'],
      ['Accounts', '/accounts'],
      ['Contracts', '/contracts'],
      ['SDEX', '/sdex'],
      ['Protocols', '/protocols'],
      ['Oracles', '/oracles'],
      ['Insights', '/insights'],
    ] as const) {
      expect(screen.getByRole('link', { name: label })).toHaveAttribute('href', href);
    }
    // External: Markets → the CEX board; Assets → external assets.
    expect(screen.getByRole('link', { name: 'Markets' })).toHaveAttribute('href', '/exchanges');
    // Two "Assets" links exist (Stellar + External) — assert both hrefs.
    const assetLinks = screen.getAllByRole('link', { name: 'Assets' });
    expect(assetLinks.map((a) => a.getAttribute('href')).sort()).toEqual([
      '/assets',
      '/external/assets',
    ]);
    // Developers
    expect(screen.getByRole('link', { name: /API Docs/ })).toHaveAttribute(
      'href',
      'https://docs.stellarindex.io',
    );
    expect(screen.getByRole('link', { name: 'SDK' })).toHaveAttribute('href', '/sdk');
    // The Status row's accessible name now includes the live tone dot's
    // sr-only state suffix (A5-03 pill revival) — match on the prefix.
    expect(screen.getByRole('link', { name: /^Status/ })).toHaveAttribute('href', '/status');
    // Retired rail entries must NOT come back silently.
    for (const gone of ['AMM Pools', 'External Markets', 'Verification', 'Home']) {
      expect(screen.queryByRole('link', { name: gone })).not.toBeInTheDocument();
    }
  });
});

// A5-03 / D2: the Status row carries a live tone dot fed by the SHARED
// useStatus query (the navbar pill was lost in the console-shell redesign
// and its hook orphaned — the rail's Status entry reflected nothing).
describe('Sidebar Status pill', () => {
  it('reflects the live /v1/status overall state on the Status row', async () => {
    vi.spyOn(globalThis, 'fetch').mockResolvedValue({
      ok: true,
      status: 200,
      json: async () => ({ data: { overall: 'degraded' } }),
    } as Response);
    renderNav();
    // The dot's sr-only state suffix lands in the link's accessible name.
    expect(await screen.findByText('(degraded performance)')).toBeInTheDocument();
  });

  it('makes NO claim when the status feed cannot be reached (WB-04 honesty)', async () => {
    // beforeEach already rejects every fetch — the pill must render the
    // muted unknown dot, never a stale/assumed green.
    renderNav();
    expect(await screen.findByText('(status unknown)')).toBeInTheDocument();
    expect(screen.queryByText('(all systems operational)')).not.toBeInTheDocument();
  });
});

describe('Sidebar AccountMenu', () => {
  it('restores focus to the trigger button after closing with Escape, even when focus had moved into the panel', () => {
    renderNav();
    const trigger = screen.getByRole('button', { name: /signed-in@example.com/ });
    trigger.focus();
    fireEvent.click(trigger);

    // Simulate the realistic case: the visitor Tabbed from the trigger to
    // a link INSIDE the open panel (the menu's whole point) before
    // dismissing it — not the vacuous case where focus never left the
    // trigger to begin with.
    const accountLink = screen.getByRole('link', { name: /Your account/ });
    accountLink.focus();
    expect(document.activeElement).toBe(accountLink);

    fireEvent.keyDown(document, { key: 'Escape' });

    expect(screen.queryByText('Sign out')).not.toBeInTheDocument();
    // Focus must return to the trigger, not fall back to <body> (which is
    // where an unmounted focused element's focus goes by default).
    expect(document.activeElement).toBe(trigger);
  });
});
