import { describe, it, expect, vi } from 'vitest';
import { render, screen, fireEvent } from '@testing-library/react';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';

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
    expect(screen.getByRole('link', { name: 'Status' })).toHaveAttribute('href', '/status');
    // Retired rail entries must NOT come back silently.
    for (const gone of ['AMM Pools', 'External Markets', 'Verification', 'Home']) {
      expect(screen.queryByRole('link', { name: gone })).not.toBeInTheDocument();
    }
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
