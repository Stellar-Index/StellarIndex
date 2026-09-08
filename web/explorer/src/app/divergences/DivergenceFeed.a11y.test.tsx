import { describe, it, expect, vi } from 'vitest';
import { render, screen, fireEvent, waitFor } from '@testing-library/react';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';

import { DivergenceFeed } from './DivergenceFeed';

vi.mock('@/api/client', async () => {
  const actual =
    await vi.importActual<typeof import('@/api/client')>('@/api/client');
  return { ...actual, apiGet: vi.fn() };
});

import { apiGet } from '@/api/client';

// Two board rows. The API orders by |Δ%| desc and the component defaults the
// selection to row 0, so asserting on row 0 would prove nothing. Every
// assertion below targets row 1 (AAA), which is reachable ONLY by an
// explicit user action.
const BOARD = {
  observations: [
    {
      asset_id: 'BBB-GB',
      quote_id: 'USD',
      reference: 'coingecko',
      our_price: '2.00',
      ref_price: '1.00',
      delta_pct: '100.00',
      observed_at: '2026-09-02T00:00:00Z',
      status: 'firing',
    },
    {
      asset_id: 'AAA-GA',
      quote_id: 'USD',
      reference: 'chainlink',
      our_price: '1.00',
      ref_price: '1.01',
      delta_pct: '-1.00',
      observed_at: '2026-09-02T00:00:00Z',
      status: 'clear',
    },
  ],
};

function mountFeed() {
  vi.mocked(apiGet).mockImplementation(async (path: string) => {
    if (path === '/v1/divergence') return { data: BOARD };
    return { data: { points: [] } };
  });
  const qc = new QueryClient({
    defaultOptions: { queries: { retry: false, refetchInterval: false } },
  });
  return render(
    <QueryClientProvider client={qc}>
      <DivergenceFeed />
    </QueryClientProvider>,
  );
}

// Testing Library's findBy* default timeout is 1000 ms, which is ample on an
// idle machine and not ample inside `make verify`: that runs four lanes
// concurrently — gitleaks alone was measured at 717 % CPU — and this board
// renders through a QueryClientProvider, so the button can appear well past a
// second under that load. It failed exactly once that way on 2026-09-08
// ("Unable to find role=button and name /Plot AAA/", 1755 ms) while passing
// 3/3 standalone and in a full 678-test standalone run, i.e. the component is
// fine and the deadline was not.
//
// Raising it is the right fix rather than a mask: the assertion is still that
// the button EXISTS with an accessible name, and a genuinely missing button
// fails just as loudly, five seconds later. A gate that goes red for a reason
// unrelated to the diff teaches people to re-run it, which is worse than slow.
const seriesButton = (label: string) =>
  screen.findByRole('button', { name: new RegExp(label) }, { timeout: 5000 });

/**
 * Sequential-focus-navigation order, computed the way a browser does: the
 * elements that are natively focusable or opted in with a non-negative
 * tabindex. jsdom does not implement Tab, so we enumerate the ring rather
 * than pretend to walk it.
 */
function tabRing(root: HTMLElement): HTMLElement[] {
  const sel =
    'a[href], button, input, select, textarea, [tabindex]:not([tabindex="-1"])';
  return Array.from(root.querySelectorAll<HTMLElement>(sel)).filter(
    (el) => !el.hasAttribute('disabled') && el.tabIndex >= 0,
  );
}

// WCAG 2.1.1 (Keyboard) + 4.1.2 (Name, Role, Value). Picking which series the
// Δ%-history chart plots is the whole job of the divergence board, and it used
// to be reachable exclusively through `<tr onClick>` — no tabIndex, no
// onKeyDown, no role — so a keyboard or screen-reader user could not change
// the chart at all.
describe('DivergenceFeed board rows are keyboard-operable', () => {
  it('exposes each row as a native button carrying its selected state', async () => {
    const { container } = mountFeed();

    const aaa = await seriesButton('Plot AAA');
    // A NATIVE <button> is what makes Enter/Space work without a hand-rolled
    // key handler; jsdom has no activation behaviour for keys, so the
    // structural property is the assertion that carries the WCAG guarantee.
    expect(aaa.tagName).toBe('BUTTON');
    expect(aaa).toHaveAttribute('type', 'button');
    expect(aaa).not.toBeDisabled();

    // In the sequential focus order, so Tab actually reaches it.
    expect(tabRing(container)).toContain(aaa);
    expect(aaa.tabIndex).toBeGreaterThanOrEqual(0);

    // Focusable in practice, not just in theory.
    aaa.focus();
    expect(aaa).toHaveFocus();

    // Selected state is exposed to AT, and row 1 is NOT the default.
    expect(aaa).toHaveAttribute('aria-current', 'false');
    expect(await seriesButton('Plot BBB')).toHaveAttribute(
      'aria-current',
      'true',
    );
  });

  it('activating the row control moves the selection to that series', async () => {
    mountFeed();

    const aaa = await seriesButton('Plot AAA');
    // Activation is exactly what Enter/Space dispatch on a native button.
    fireEvent.click(aaa);

    await waitFor(() => {
      expect(screen.getByRole('button', { name: /Plot AAA/ })).toHaveAttribute(
        'aria-current',
        'true',
      );
    });
    // ...and the previously-selected row gives the state up, so exactly one
    // row is ever pressed.
    expect(screen.getByRole('button', { name: /Plot BBB/ })).toHaveAttribute(
      'aria-current',
      'false',
    );
  });

  it('keeps the selection single-valued across repeated activation', async () => {
    const { container } = mountFeed();

    fireEvent.click(await seriesButton('Plot AAA'));
    await waitFor(() =>
      expect(screen.getByRole('button', { name: /Plot AAA/ })).toHaveAttribute(
        'aria-current',
        'true',
      ),
    );
    fireEvent.click(screen.getByRole('button', { name: /Plot BBB/ }));

    await waitFor(() =>
      expect(screen.getByRole('button', { name: /Plot BBB/ })).toHaveAttribute(
        'aria-current',
        'true',
      ),
    );
    const pressed = tabRing(container).filter(
      (el) => el.getAttribute('aria-current') === 'true',
    );
    expect(pressed).toHaveLength(1);
  });
});
