import { describe, it, expect } from 'vitest';
import { render, screen, fireEvent } from '@testing-library/react';

import { CurrencyCombobox } from './CurrencyCombobox';

// ACC-06: CurrencyCombobox was a hand-rolled dropdown with no focus-trap
// or focus-restore — closing it (Escape) never returned focus to the
// trigger button, unlike the shared useDialog hook already used elsewhere
// (RequestReveal).
describe('CurrencyCombobox', () => {
  it('restores focus to the trigger button after closing with Escape, even when focus had moved into the panel', () => {
    render(
      <CurrencyCombobox tickers={['USD', 'EUR', 'GBP']} value="USD" onChange={() => {}} />,
    );
    const trigger = screen.getByRole('button', { name: /USD/ });
    trigger.focus();
    fireEvent.click(trigger);

    // Simulate focus having moved into the open panel (the point of a
    // combobox) before the visitor dismisses it — not the vacuous case
    // where focus never left the trigger.
    const eurOption = screen.getByRole('button', { name: /EUR/ });
    eurOption.focus();
    expect(document.activeElement).toBe(eurOption);

    fireEvent.keyDown(document, { key: 'Escape' });

    expect(screen.queryByRole('dialog')).not.toBeInTheDocument();
    // Focus must return to the trigger, not fall back to <body> (which is
    // where an unmounted focused element's focus goes by default).
    expect(document.activeElement).toBe(trigger);
  });

  it('marks the open panel as a dialog for assistive tech', () => {
    render(
      <CurrencyCombobox tickers={['USD', 'EUR']} value="USD" onChange={() => {}} />,
    );
    fireEvent.click(screen.getByRole('button', { name: /USD/ }));
    expect(screen.getByRole('dialog')).toHaveAttribute('aria-modal', 'true');
  });
});
