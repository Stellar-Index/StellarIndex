'use client';

import { useEffect, useRef } from 'react';

// FEC audit A6-2: stacked dialogs (e.g. mobile nav drawer -> search modal).
// Escape must close only the TOPMOST dialog; because document-level keydown
// listeners fire in registration order, the bottom dialog's handler runs
// first and would also close without this stack.
const dialogStack: symbol[] = [];

const FOCUSABLE = [
  'a[href]',
  'button:not([disabled])',
  'textarea:not([disabled])',
  'input:not([disabled])',
  'select:not([disabled])',
  '[tabindex]:not([tabindex="-1"])',
].join(',');

/**
 * useDialog wires the WCAG modal-dialog contract that every reveal/drawer was
 * missing (LC-050 / LC-051):
 *
 *  - **Escape** closes the dialog.
 *  - **Focus moves in** when it opens (first focusable, else the container).
 *  - **Focus is trapped** — Tab / Shift-Tab wrap within the dialog.
 *  - **Focus is restored** to whatever was focused before (the trigger) on close.
 *
 * Attach the returned ref to the dialog container (give it `tabIndex={-1}` so it
 * can receive focus, `role="dialog"` and `aria-modal="true"`). `onClose` MUST be
 * stable (wrap in useCallback) or the effect re-runs and re-steals focus.
 */
export function useDialog<T extends HTMLElement>(
  open: boolean,
  onClose: () => void,
) {
  const ref = useRef<T>(null);
  const restoreRef = useRef<HTMLElement | null>(null);

  useEffect(() => {
    if (!open) return;

    const node = ref.current;
    restoreRef.current = (document.activeElement as HTMLElement) ?? null;
    const stackToken = Symbol('dialog');
    dialogStack.push(stackToken);

    const focusables = (): HTMLElement[] =>
      node
        ? Array.from(node.querySelectorAll<HTMLElement>(FOCUSABLE)).filter(
            (el) => el.offsetParent !== null,
          )
        : [];

    // Move focus into the dialog.
    (focusables()[0] ?? node)?.focus();

    const onKeyDown = (e: KeyboardEvent) => {
      if (e.key === 'Escape') {
        // Only the topmost dialog in the stack responds (A6-2).
        if (dialogStack[dialogStack.length - 1] !== stackToken) return;
        e.preventDefault();
        onClose();
        return;
      }
      if (e.key !== 'Tab' || !node) return;
      const items = focusables();
      if (items.length === 0) {
        e.preventDefault();
        node.focus();
        return;
      }
      const first = items[0];
      const last = items[items.length - 1];
      const active = document.activeElement;
      if (e.shiftKey && (active === first || active === node)) {
        e.preventDefault();
        last.focus();
      } else if (!e.shiftKey && active === last) {
        e.preventDefault();
        first.focus();
      }
    };

    document.addEventListener('keydown', onKeyDown);
    return () => {
      document.removeEventListener('keydown', onKeyDown);
      const ix = dialogStack.indexOf(stackToken);
      if (ix !== -1) dialogStack.splice(ix, 1);
      // FEC audit A6-3: restore focus ONLY if the user hasn't already
      // focused something outside the dialog. The popover consumers
      // (CurrencyCombobox, sidebar AccountMenu) close on outside-mousedown
      // with no backdrop — the browser focuses the clicked control, and an
      // unconditional restore here yanked focus back to the trigger
      // (keystrokes lost). Modals with a backdrop are unaffected: focus at
      // close is inside the dialog (or on body after unmount), so the
      // restore still runs.
      const active = document.activeElement;
      if (
        !active ||
        active === document.body ||
        (node ? node.contains(active) : false)
      ) {
        restoreRef.current?.focus?.();
      }
    };
  }, [open, onClose]);

  return ref;
}
