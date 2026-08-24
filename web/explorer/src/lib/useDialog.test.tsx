import { describe, it, expect } from 'vitest';
import { fireEvent, render, screen } from '@testing-library/react';
import { useCallback, useState } from 'react';

import { useDialog } from './useDialog';

// FEC audit A6-3: useDialog restored focus to the pre-open trigger on EVERY
// close. Its popover consumers (CurrencyCombobox, sidebar AccountMenu) close
// on outside-mousedown with no backdrop, so clicking from an open popover
// straight into another control had focus yanked back off that control.
// The fix restores only when focus is still inside the dialog / on body.

function Harness() {
  const [open, setOpen] = useState(false);
  const close = useCallback(() => setOpen(false), []);
  const ref = useDialog<HTMLDivElement>(open, close);
  return (
    <div>
      <button type="button" onClick={() => setOpen(true)}>
        trigger
      </button>
      <input aria-label="outside" />
      {open && (
        <div
          ref={ref}
          role="dialog"
          aria-modal="true"
          aria-label="dlg"
          tabIndex={-1}
        >
          <button type="button" onClick={close}>
            inside
          </button>
        </div>
      )}
    </div>
  );
}

describe('useDialog focus restore (A6-3)', () => {
  it('restores focus to the trigger when closed via Escape', () => {
    render(<Harness />);
    const trigger = screen.getByRole('button', { name: 'trigger' });
    trigger.focus();
    fireEvent.click(trigger);
    expect(screen.getByRole('dialog')).toBeInTheDocument();
    fireEvent.keyDown(document, { key: 'Escape' });
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument();
    expect(document.activeElement).toBe(trigger);
  });

  it('does NOT steal focus from a control the user moved to outside the dialog', () => {
    render(<Harness />);
    const trigger = screen.getByRole('button', { name: 'trigger' });
    trigger.focus();
    fireEvent.click(trigger);
    expect(screen.getByRole('dialog')).toBeInTheDocument();
    // Popover pattern: user clicks/focuses an outside control; a
    // click-outside handler then closes the dialog.
    const outside = screen.getByLabelText('outside');
    outside.focus();
    fireEvent.keyDown(document, { key: 'Escape' });
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument();
    expect(document.activeElement).toBe(outside);
  });
});

// FEC audit A6-2: with the mobile drawer AND the search modal open (both on
// document-level keydown), one Escape used to close BOTH — the user's drawer
// vanished underneath the search they meant to dismiss. The dialogStack means
// Escape closes only the topmost; a second Escape then reaches the one below.
function StackHarness() {
  const [drawerOpen, setDrawerOpen] = useState(true);
  const [searchOpen, setSearchOpen] = useState(true);
  const closeDrawer = useCallback(() => setDrawerOpen(false), []);
  const closeSearch = useCallback(() => setSearchOpen(false), []);
  const drawerRef = useDialog<HTMLDivElement>(drawerOpen, closeDrawer);
  const searchRef = useDialog<HTMLDivElement>(searchOpen, closeSearch);
  return (
    <div>
      {drawerOpen && (
        <div ref={drawerRef} role="dialog" aria-label="drawer" tabIndex={-1} />
      )}
      {searchOpen && (
        <div ref={searchRef} role="dialog" aria-label="search" tabIndex={-1} />
      )}
    </div>
  );
}

describe('useDialog stacking (A6-2)', () => {
  it('Escape closes only the topmost dialog; the one below survives', () => {
    render(<StackHarness />);
    expect(screen.getByRole('dialog', { name: 'drawer' })).toBeInTheDocument();
    expect(screen.getByRole('dialog', { name: 'search' })).toBeInTheDocument();

    // search mounted second → top of the stack → the only one Escape closes.
    fireEvent.keyDown(document, { key: 'Escape' });
    expect(
      screen.queryByRole('dialog', { name: 'search' }),
    ).not.toBeInTheDocument();
    expect(screen.getByRole('dialog', { name: 'drawer' })).toBeInTheDocument();

    // The next Escape reaches the drawer, now topmost.
    fireEvent.keyDown(document, { key: 'Escape' });
    expect(
      screen.queryByRole('dialog', { name: 'drawer' }),
    ).not.toBeInTheDocument();
  });
});
