import { describe, it, expect } from 'vitest';
import { render } from '@testing-library/react';

import { RollingNumber } from './RollingNumber';

// Odometer contract: a re-render with a changed value remounts (and so
// animates) exactly the characters that changed — the carried span —
// while untouched digits keep their DOM nodes (no replay, no jitter).
describe('RollingNumber', () => {
  function digitNodes(container: HTMLElement) {
    return Array.from(container.querySelectorAll('.digit-roll'));
  }

  it('renders the formatted value (screen-reader copy + per-char spans)', () => {
    const { container, getByText } = render(<RollingNumber value={1234567} />);
    expect(getByText('1,234,567')).toBeInTheDocument(); // sr-only copy
    expect(digitNodes(container).map((n) => n.textContent).join('')).toBe('1,234,567');
  });

  it('keeps unchanged leading digits mounted and remounts only the changed tail', () => {
    const { container, rerender } = render(<RollingNumber value={59123456} />);
    const before = digitNodes(container);
    rerender(<RollingNumber value={59123457} />);
    const after = digitNodes(container);
    // "59,123,456" -> "59,123,457": everything up to the last char is the
    // same node; the final digit is a fresh mount (new key -> animation).
    for (let i = 0; i < before.length - 1; i++) expect(after[i]).toBe(before[i]);
    expect(after[after.length - 1]).not.toBe(before[before.length - 1]);
  });

  it('rolls the whole carry span on 9->10 style carries', () => {
    const { container, rerender } = render(<RollingNumber value={59123999} />);
    const before = digitNodes(container);
    rerender(<RollingNumber value={59124000} />);
    const after = digitNodes(container);
    // "59,123,999" -> "59,124,000": chars 0-4 ("59,12") unchanged; index 5
    // and the three trailing digits changed; the comma at index 6 stays.
    for (let i = 0; i <= 4; i++) expect(after[i]).toBe(before[i]);
    expect(after[5]).not.toBe(before[5]);
    expect(after[6]).toBe(before[6]); // comma
    for (let i = 7; i <= 9; i++) expect(after[i]).not.toBe(before[i]);
  });

  it('remounts everything when the length changes (99 -> 100)', () => {
    const { container, rerender } = render(<RollingNumber value={99} />);
    const before = digitNodes(container);
    rerender(<RollingNumber value={100} />);
    const after = digitNodes(container);
    expect(after.length).toBe(3);
    for (const node of after) expect(before).not.toContain(node);
  });
});
