import { describe, it, expect } from 'vitest';
import { render } from '@testing-library/react';

import { RollingNumber } from './RollingNumber';

// Odometer contract (v3 — true rollover): a re-render with a changed
// value remounts exactly the changed columns as two-digit strips
// ([old, new] in a clipped window: the old character visibly exits
// upward, the new arrives from below), while untouched digits keep
// their DOM nodes (no replay, no jitter). The FIRST render of a mount
// animates nothing: page load and the badge's unmount/remount around a
// stream hiccup must not flash the whole number.
describe('RollingNumber', () => {
  function cells(container: HTMLElement) {
    return Array.from(container.querySelectorAll('.digit-cell'));
  }
  // Rolling cells: static digits now share the .digit-window box (for exact
  // vertical alignment), so a *rolling* cell is identified by its animating
  // .digit-strip child (a static cell has a .digit-flat instead).
  function windows(container: HTMLElement) {
    return Array.from(container.querySelectorAll('.digit-cell')).filter((c) =>
      c.querySelector('.digit-strip'),
    );
  }
  // The character a cell currently SHOWS: a plain cell's text, or a
  // strip's second (new) digit.
  function shown(cell: Element) {
    const strip = cell.querySelector('.digit-strip');
    return strip ? strip.children[1].textContent : cell.textContent;
  }
  function stripPair(cell: Element) {
    const strip = cell.querySelector('.digit-strip');
    return strip
      ? [strip.children[0].textContent, strip.children[1].textContent]
      : null;
  }

  it('renders the formatted value (screen-reader copy + per-char cells)', () => {
    const { container, getByText } = render(<RollingNumber value={1234567} />);
    expect(getByText('1,234,567')).toBeInTheDocument(); // sr-only copy
    expect(cells(container).map(shown).join('')).toBe('1,234,567');
  });

  it('animates NOTHING on first mount — no page-load / remount flash', () => {
    const { container } = render(<RollingNumber value={59123456} />);
    expect(cells(container).length).toBe(10);
    expect(windows(container).length).toBe(0);
  });

  it('keeps unchanged leading digits mounted and rolls only the changed tail', () => {
    const { container, rerender } = render(<RollingNumber value={59123456} />);
    const before = cells(container);
    rerender(<RollingNumber value={59123457} />);
    const after = cells(container);
    // "59,123,456" -> "59,123,457": everything up to the last char is
    // the same node; the final column is a fresh rollover window whose
    // strip carries old THEN new — the wheel.
    for (let i = 0; i < before.length - 1; i++) expect(after[i]).toBe(before[i]);
    expect(after[after.length - 1]).not.toBe(before[before.length - 1]);
    expect(windows(container)).toEqual([after[after.length - 1]]);
    expect(stripPair(after[after.length - 1])).toEqual(['6', '7']);
    expect(cells(container).map(shown).join('')).toBe('59,123,457');
  });

  it('rolls the whole carry span on 9->10 style carries', () => {
    const { container, rerender } = render(<RollingNumber value={59123999} />);
    const before = cells(container);
    rerender(<RollingNumber value={59124000} />);
    const after = cells(container);
    // "59,123,999" -> "59,124,000": chars 0-4 ("59,12") unchanged;
    // index 5 and the three trailing digits roll; the comma at index 6
    // stays.
    for (let i = 0; i <= 4; i++) expect(after[i]).toBe(before[i]);
    expect(after[6]).toBe(before[6]); // comma
    expect(windows(container)).toEqual([after[5], after[7], after[8], after[9]]);
    expect(stripPair(after[5])).toEqual(['3', '4']);
    for (const i of [7, 8, 9]) expect(stripPair(after[i])).toEqual(['9', '0']);
    expect(cells(container).map(shown).join('')).toBe('59,124,000');
  });

  it('a digit changing twice in a row rolls both times (fresh strip per change)', () => {
    const { container, rerender } = render(<RollingNumber value={101} />);
    rerender(<RollingNumber value={102} />);
    const second = cells(container)[2];
    expect(stripPair(second)).toEqual(['1', '2']);
    rerender(<RollingNumber value={103} />);
    const third = cells(container)[2];
    expect(third).not.toBe(second);
    expect(stripPair(third)).toEqual(['2', '3']);
  });

  it('a same-value rerender retains every node — no remount, no replay', () => {
    const { container, rerender } = render(<RollingNumber value={19} />);
    rerender(<RollingNumber value={29} />); // head digit rolls
    const after = cells(container);
    expect(windows(container)).toEqual([after[0]]);
    rerender(<RollingNumber value={29} />); // value unchanged
    // Same DOM nodes throughout: a CSS animation replays only on
    // insertion, so retained nodes cannot re-roll.
    expect(cells(container)).toEqual(after);
  });

  it('a brand-new head column (999 -> 1,000) appears without a bogus roll', () => {
    const { container, rerender } = render(<RollingNumber value={999} />);
    rerender(<RollingNumber value={1000} />);
    // "999" -> "1,000": the new head "1" and "," have no previous
    // character to roll FROM — they must be plain cells; the three 9->0
    // columns roll.
    expect(cells(container).map(shown).join('')).toBe('1,000');
    expect(windows(container).length).toBe(3);
    expect(stripPair(cells(container)[0])).toBeNull();
    expect(stripPair(cells(container)[1])).toBeNull();
  });
});
