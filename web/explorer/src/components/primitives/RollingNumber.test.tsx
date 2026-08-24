import { describe, it, expect } from 'vitest';
import { render } from '@testing-library/react';

import { RollingNumber } from './RollingNumber';

// Odometer contract (v2): a re-render with a changed value remounts (and
// so animates) exactly the characters that changed — the carried span —
// while untouched digits keep their DOM nodes (no replay, no jitter).
// The FIRST render of a mount animates nothing: page load and the
// badge's unmount/remount around a stream hiccup must not flash the
// whole number (the 2026-08-24 operator report).
describe('RollingNumber', () => {
  function cells(container: HTMLElement) {
    return Array.from(container.querySelectorAll('.digit-cell'));
  }
  function rolling(container: HTMLElement) {
    return Array.from(container.querySelectorAll('.digit-roll'));
  }

  it('renders the formatted value (screen-reader copy + per-char spans)', () => {
    const { container, getByText } = render(<RollingNumber value={1234567} />);
    expect(getByText('1,234,567')).toBeInTheDocument(); // sr-only copy
    expect(cells(container).map((n) => n.textContent).join('')).toBe('1,234,567');
  });

  it('animates NOTHING on first mount — no page-load / remount flash', () => {
    const { container } = render(<RollingNumber value={59123456} />);
    expect(cells(container).length).toBe(10);
    expect(rolling(container).length).toBe(0);
  });

  it('keeps unchanged leading digits mounted and rolls only the changed tail', () => {
    const { container, rerender } = render(<RollingNumber value={59123456} />);
    const before = cells(container);
    rerender(<RollingNumber value={59123457} />);
    const after = cells(container);
    // "59,123,456" -> "59,123,457": everything up to the last char is the
    // same node; the final digit is a fresh mount carrying the animation.
    for (let i = 0; i < before.length - 1; i++) expect(after[i]).toBe(before[i]);
    expect(after[after.length - 1]).not.toBe(before[before.length - 1]);
    expect(rolling(container)).toEqual([after[after.length - 1]]);
  });

  it('rolls the whole carry span on 9->10 style carries', () => {
    const { container, rerender } = render(<RollingNumber value={59123999} />);
    const before = cells(container);
    rerender(<RollingNumber value={59124000} />);
    const after = cells(container);
    // "59,123,999" -> "59,124,000": chars 0-4 ("59,12") unchanged; index 5
    // and the three trailing digits changed; the comma at index 6 stays.
    for (let i = 0; i <= 4; i++) expect(after[i]).toBe(before[i]);
    expect(after[5]).not.toBe(before[5]);
    expect(after[6]).toBe(before[6]); // comma
    for (let i = 7; i <= 9; i++) expect(after[i]).not.toBe(before[i]);
    expect(rolling(container)).toEqual([after[5], after[7], after[8], after[9]]);
  });

  it('a digit changing twice in a row rolls both times (fresh node per change)', () => {
    const { container, rerender } = render(<RollingNumber value={101} />);
    rerender(<RollingNumber value={102} />);
    const second = cells(container)[2];
    expect(rolling(container)).toEqual([second]);
    rerender(<RollingNumber value={103} />);
    const third = cells(container)[2];
    expect(third).not.toBe(second);
    expect(rolling(container)).toEqual([third]);
  });

  it('a same-value rerender retains every node — no remount, no replay', () => {
    const { container, rerender } = render(<RollingNumber value={19} />);
    rerender(<RollingNumber value={29} />); // head digit rolls
    const after = cells(container);
    expect(rolling(container)).toEqual([after[0]]);
    rerender(<RollingNumber value={29} />); // value unchanged
    // Same DOM nodes throughout: a CSS animation replays only on
    // insertion, so retained nodes cannot flash even if the class
    // lingers from the last change.
    expect(cells(container)).toEqual(after);
  });
});
