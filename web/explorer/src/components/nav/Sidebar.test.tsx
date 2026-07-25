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
