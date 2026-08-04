import { describe, it, expect } from 'vitest';
import { render, screen, fireEvent } from '@testing-library/react';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';

import { SearchModal, search } from './SearchModal';

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

// The ⌘K result builder is the fourth copy of the verified-badge rule
// (AssetsTable / HomeTopAssets / HomeTopMovers are the other three) and
// was the only one that did not consult unverified_ticker_collision. The
// listing serves COALESCE(slug, code) AS slug, so an impersonator emits
// the verified asset's CODE as its slug and matches the verified set —
// meaning an asset the backend had explicitly flagged rendered with the
// "Verified currency" check in search. Cold audit 2026-08-04.
describe('SearchModal verified badge', () => {
  const verifiedSlugs = new Set(['aqua']);
  const coin = (over: Record<string, unknown>) =>
    ({
      asset_id: 'AQUA-GTEST',
      slug: 'AQUA',
      name: 'Aquarius',
      symbol: 'AQUA',
      unverified_ticker_collision: false,
      ...over,
    }) as never;

  it('badges the real verified asset', () => {
    const [r] = search('aqua', [coin({})], [], false, verifiedSlugs);
    expect(r.verified).toBe(true);
  });

  it('does NOT badge an impersonator the API flagged, even though its slug matches', () => {
    const [r] = search(
      'aqua',
      [coin({ unverified_ticker_collision: true })],
      [],
      false,
      verifiedSlugs,
    );
    expect(r.verified).toBe(false);
  });
});
