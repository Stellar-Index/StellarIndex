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
      <CurrencyCombobox
        tickers={['USD', 'EUR', 'GBP']}
        value="USD"
        onChange={() => {}}
      />,
    );
    const trigger = screen.getByRole('button', { name: /USD/ });
    trigger.focus();
    fireEvent.click(trigger);

    // Simulate focus having moved into the open panel (the point of a
    // combobox) before the visitor dismisses it — not the vacuous case
    // where focus never left the trigger.
    // Located by role="option" (not "button"): the rows carry listbox
    // option semantics since the #335-F7b ARIA fix. The element and the
    // focus-restore assertions below are otherwise unchanged.
    const eurOption = screen.getByRole('option', { name: /EUR/ });
    eurOption.focus();
    expect(document.activeElement).toBe(eurOption);

    fireEvent.keyDown(document, { key: 'Escape' });

    expect(screen.queryByRole('dialog')).not.toBeInTheDocument();
    // Focus must return to the trigger, not fall back to <body> (which is
    // where an unmounted focused element's focus goes by default).
    expect(document.activeElement).toBe(trigger);
  });

  it('exposes the open panel as a labelled non-modal popover (A6-3)', () => {
    render(
      <CurrencyCombobox
        tickers={['USD', 'EUR']}
        value="USD"
        onChange={() => {}}
      />,
    );
    fireEvent.click(screen.getByRole('button', { name: /USD/ }));
    // The page behind the panel stays interactive, so it must NOT claim
    // aria-modal/dialog semantics (that tells AT the rest of the page is
    // inert — false here). It stays labelled for AT context.
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument();
    const panel = screen.getByLabelText(/currency/i);
    expect(panel).not.toHaveAttribute('aria-modal');
  });

  // #335 F7b (WCAG 4.1.2 Name, Role, Value): the trigger announced no
  // expanded/collapsed state and the option rows were bare buttons, so a
  // screen reader could not tell the control was a picker, how many
  // choices it offered, or which one was current.
  it('announces the trigger as an expandable listbox owner', () => {
    render(
      <CurrencyCombobox
        tickers={['USD', 'EUR']}
        value="USD"
        onChange={() => {}}
      />,
    );
    const trigger = screen.getByRole('button', { name: /USD/ });
    expect(trigger).toHaveAttribute('aria-haspopup', 'listbox');
    expect(trigger).toHaveAttribute('aria-expanded', 'false');
    // Nothing to control while collapsed — a dangling idref is invalid.
    expect(trigger).not.toHaveAttribute('aria-controls');

    fireEvent.click(trigger);
    expect(trigger).toHaveAttribute('aria-expanded', 'true');

    // The trigger's aria-controls must resolve to the real listbox node.
    const listbox = screen.getByRole('listbox');
    expect(trigger.getAttribute('aria-controls')).toBe(listbox.id);
    expect(listbox.id).toBeTruthy();
  });

  it('exposes each currency as an option and marks the current one selected', () => {
    render(
      <CurrencyCombobox
        tickers={['USD', 'EUR', 'GBP']}
        value="EUR"
        onChange={() => {}}
      />,
    );
    fireEvent.click(screen.getByRole('button', { name: /EUR/ }));

    const options = screen.getAllByRole('option');
    expect(options.map((o) => o.textContent?.trim())).toEqual([
      'USD',
      'EURcurrent',
      'GBP',
    ]);
    // Exactly the current value is selected — not none, not all.
    expect(screen.getByRole('option', { name: /EUR/ })).toHaveAttribute(
      'aria-selected',
      'true',
    );
    expect(screen.getByRole('option', { name: /USD/ })).toHaveAttribute(
      'aria-selected',
      'false',
    );
    expect(
      options.filter((o) => o.getAttribute('aria-selected') === 'true'),
    ).toHaveLength(1);
  });

  it('points the search box at the highlighted option via aria-activedescendant', () => {
    render(
      <CurrencyCombobox
        tickers={['USD', 'EUR', 'GBP']}
        value="USD"
        onChange={() => {}}
      />,
    );
    fireEvent.click(screen.getByRole('button', { name: /USD/ }));

    const search = screen.getByRole('combobox');
    const options = screen.getAllByRole('option');
    // Highlight starts at the top of the filtered list.
    expect(search).toHaveAttribute('aria-activedescendant', options[0].id);

    fireEvent.keyDown(search, { key: 'ArrowDown' });
    expect(search).toHaveAttribute('aria-activedescendant', options[1].id);
  });
});
