import { describe, it, expect, vi } from 'vitest';
import { render, screen, fireEvent, waitFor } from '@testing-library/react';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';

import { DivergenceFeed, referenceLines } from './DivergenceFeed';

vi.mock('@/api/client', async () => {
  const actual =
    await vi.importActual<typeof import('@/api/client')>('@/api/client');
  return { ...actual, apiGet: vi.fn() };
});

import { apiGet } from '@/api/client';

// Two board pairs. The API orders pairs by widest |Δ%| desc and the
// component defaults the selection to pair 0, so asserting on pair 0 would
// prove nothing. Every assertion below targets pair 1 (AAA), which is
// reachable ONLY by an explicit user action.
const BOARD = {
  pairs: [
    {
      asset_id: 'BBB-GB',
      quote_id: 'USD',
      our_price: '2.00',
      observed_at: '2026-09-02T00:00:00Z',
      observed_at_ledger: 0,
      references: [
        {
          reference: 'coingecko',
          ref_price: '1.00',
          delta_pct: '100.00',
          status: 'firing',
          observed_at: '2026-09-02T00:00:00Z',
          ref_observed_at: null,
        },
        {
          reference: 'chainlink',
          ref_price: '1.98',
          delta_pct: '1.01',
          status: 'clear',
          observed_at: '2026-09-02T00:00:00Z',
          ref_observed_at: null,
        },
      ],
    },
    {
      asset_id: 'AAA-GA',
      quote_id: 'USD',
      our_price: '1.00',
      observed_at: '2026-09-02T00:00:00Z',
      observed_at_ledger: 0,
      references: [
        {
          reference: 'chainlink',
          ref_price: '1.01',
          delta_pct: '-1.00',
          status: 'clear',
          observed_at: '2026-09-02T00:00:00Z',
          ref_observed_at: null,
        },
      ],
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
// second under that load. It failed exactly once that way
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

// REGRESSION: the board's column headers were bare <th> with no
// scope, so a screen reader announcing a data cell never names which column
// it belongs to.
describe('DivergenceFeed table header cells declare their scope', () => {
  it('every named column header is scope="col"', async () => {
    mountFeed();
    await seriesButton('Plot AAA');

    const headers = screen.getAllByRole('columnheader');
    // The trailing aria-hidden spacer column carries no accessible name and
    // is intentionally excluded — scope has nothing to attach to there.
    const named = headers.filter((h) => h.textContent?.trim());
    expect(named.length).toBeGreaterThan(0);
    for (const h of named) {
      expect(h).toHaveAttribute('scope', 'col');
    }
  });
});

// The owner rule: a reference's price is never served or shown on its own.
// The board groups every reference under its pair beside our price, and
// the history plots one line per reference rather than one chosen alone.
describe('DivergenceFeed shows references only beside each other', () => {
  it('lists every reference of a pair under that pair', async () => {
    mountFeed();
    await seriesButton('Plot BBB');
    expect(screen.getAllByText('coingecko').length).toBeGreaterThan(0);
    // chainlink appears under both pairs.
    expect(screen.getAllByText('chainlink').length).toBeGreaterThanOrEqual(2);
    expect(screen.getAllByRole('button', { name: /^Plot / })).toHaveLength(2);
  });

  it('requests the series by pair only, never by reference', async () => {
    mountFeed();
    await seriesButton('Plot BBB');
    await waitFor(() =>
      expect(vi.mocked(apiGet)).toHaveBeenCalledWith(
        '/v1/divergence/series',
        expect.anything(),
      ),
    );
    for (const [path, params] of vi.mocked(apiGet).mock.calls) {
      if (path === '/v1/divergence/series') {
        expect(params).not.toHaveProperty('reference');
      }
    }
  });

  it('builds one gap-aware line per reference', () => {
    const lines = referenceLines([
      {
        t: '2026-09-02T00:00:00Z',
        our_price: '2',
        references: [
          { reference: 'coingecko', ref_price: '1', delta_pct: '100' },
          { reference: 'chainlink', ref_price: '1.98', delta_pct: '1.01' },
        ],
      },
      {
        t: '2026-09-02T00:30:00Z',
        our_price: '2',
        references: [
          { reference: 'chainlink', ref_price: '2', delta_pct: '0' },
        ],
      },
    ]);
    expect(lines.map((l) => l.label)).toEqual(['coingecko', 'chainlink']);
    expect(lines[0].data.map((p) => p.value)).toEqual([100, null]);
    expect(lines[1].data.map((p) => p.value)).toEqual([1.01, 0]);
    expect(lines[0].color).not.toEqual(lines[1].color);
  });
});
