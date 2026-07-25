import { describe, it, expect } from 'vitest';
import { render, screen, fireEvent } from '@testing-library/react';

import { RequestReveal } from './RequestReveal';

// ACC-21: the reveal tray's "Close" button was a bare 16x16 icon with no
// padding — below the WCAG 2.2 SC 2.5.8 24x24 CSS-px minimum target size.
// The test suite doesn't load real CSS (`css: false` in vitest.config.ts),
// so the only way to pin the fix here is the padding utility class itself:
// p-1 (4px/side) around a 16px (h-4 w-4) icon reaches exactly 24x24.
describe('RequestReveal', () => {
  it('gives the Close button >=24x24 CSS px of hit area (icon + padding)', () => {
    render(<RequestReveal example={{ method: 'GET', url: 'https://api.stellarindex.io/v1/price' }} />);
    fireEvent.click(screen.getByRole('button', { name: 'Show API request' }));
    const closeBtn = screen.getByRole('button', { name: 'Close' });
    expect(closeBtn.className).toMatch(/(^|\s)p-(1(\.5)?|2)(\s|$)/);
  });
});
