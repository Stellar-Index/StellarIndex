import { describe, it, expect } from 'vitest';
import { render, screen, fireEvent } from '@testing-library/react';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';

import { SearchModal } from './SearchModal';

// ACC-21: the modal's "Close" button was a bare 16x16 icon with no padding
// — below the WCAG 2.2 SC 2.5.8 24x24 CSS-px minimum target size. The test
// suite doesn't load real CSS (`css: false` in vitest.config.ts), so the
// only way to pin the fix here is the padding utility class itself: p-1
// (4px/side) around a 16px (h-4 w-4) icon reaches exactly 24x24.
function renderOpen() {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  render(
    <QueryClientProvider client={client}>
      <SearchModal />
    </QueryClientProvider>,
  );
  fireEvent.click(screen.getByRole('button', { name: 'Open search' }));
}

describe('SearchModal', () => {
  it('gives the Close button >=24x24 CSS px of hit area (icon + padding)', () => {
    renderOpen();
    const closeBtn = screen.getByRole('button', { name: 'Close' });
    // h-4/w-4 icon = 16px; p-1 = 4px padding per side -> 16 + 4*2 = 24px.
    expect(closeBtn.className).toMatch(/(^|\s)p-(1(\.5)?|2)(\s|$)/);
  });
});
