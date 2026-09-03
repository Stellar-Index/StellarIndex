// Guard: page-level sections render an <h2>, not an <h3> (#486, WCAG 1.3.1).
//
// The built-export scan (lib/heading-order.built.test.ts) is the wide net,
// but it can only see what the STATIC HTML contains. Most of the explorer's
// sections are client components that render their real content after
// hydration — /assets/[slug]'s tab panels, the issuer panel, the venue
// tables — so a skipped level there is invisible to a scan of `out/` and
// visible to every actual screen-reader user. This file renders those
// components for real, inside a page shell that has the page's <h1>, and
// asserts the outline never jumps a level.
//
// It is deliberately written against the SHIPPED components (not a fixture
// of their markup): the regression this guards against is someone adding a
// <Panel> as a page's top-level section and inheriting its default <h3>.
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { render } from '@testing-library/react';
import type { ReactNode } from 'react';
import { describe, expect, it, vi } from 'vitest';

import {
  extractHeadings,
  formatOutline,
  formatViolation,
  headingOrderViolations,
} from '@/lib/heading-order';

import { AssetOraclesPanel } from './assets/[slug]/AssetOraclesPanel';
import { AssetSwap } from './assets/[slug]/AssetSwap';
import { HoldersTabPanel } from './assets/[slug]/HoldersTabPanel';
import { IssuerPanel } from './assets/[slug]/IssuerPanel';
import { SupplyTabPanel } from './assets/[slug]/SupplyTabPanel';
import { PoolReserves } from './lending/[pool]/PoolReserves';
import { SourceBreakdown } from './markets/[pair]/SourceBreakdown';
import { IssuersTable } from './issuers/IssuersTable';
import { SourcesTable } from './sources/SourcesTable';
import { EmptyState } from '@/components/ui';
import { VenueMarketsTable } from '@/components/VenueMarketsTable';

vi.mock('@/api/client', async () => {
  const actual =
    await vi.importActual<typeof import('@/api/client')>('@/api/client');
  // Never resolves: every component below is exercised in the state the
  // static export ships (data pending). The heading a section renders is
  // the same in every state — that is the point of the finding — and this
  // keeps the guard hermetic and fast.
  return { ...actual, apiGet: vi.fn(() => new Promise(() => {})) };
});

const G = 'GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN';
const POOL = 'CADR6Q2UOCDJAGXMAB2E6SRT35STLZ2IGLZUCXJQG7TC2LNKCU5RTQVY';

/** A page shell: the real page <h1>, then the section under test. */
function renderInPage(children: ReactNode) {
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  });
  return render(
    <QueryClientProvider client={client}>
      <h1>Page title</h1>
      {children}
    </QueryClientProvider>,
  );
}

function expectNoSkip(html: string, minHeadings = 1) {
  const headings = extractHeadings(html);
  expect(
    headings.length,
    'the section rendered no heading at all — the assertion would be vacuous',
  ).toBeGreaterThanOrEqual(minHeadings);
  const violations = headingOrderViolations(headings);
  expect(
    violations.map(formatViolation),
    `outline: ${formatOutline(headings)}`,
  ).toEqual([]);
}

describe('page-level sections do not skip a heading level', () => {
  it.each([
    [
      '/assets/[slug] — converter (sidebar)',
      <AssetSwap key="swap" symbol="USDC" assetId="USDC-G…" priceUSD={1} />,
    ],
    ['/assets/[slug] — issuer panel', <IssuerPanel key="issuer" gStrkey={G} />],
    [
      '/assets/[slug] — supply tab',
      <SupplyTabPanel key="supply" assetID="USDC-G…" />,
    ],
    [
      '/assets/[slug] — oracles tab',
      <AssetOraclesPanel key="oracles" assetID="USDC-G…" symbol="USDC" />,
    ],
    [
      '/assets/[slug] — oracles tab (ticker collision)',
      <AssetOraclesPanel
        key="oracles-collision"
        assetID="USDC-G…"
        symbol="USDC"
        tickerCollision={{
          verified_slug: 'usdc',
          verified_asset_id: `USDC-${G}`,
          verified_name: 'USD Coin',
          verified_issuer: 'Circle (centre.io)',
          note: 'Exercise caution …',
        }}
      />,
    ],
    [
      '/assets/[slug] — holders tab',
      <HoldersTabPanel key="holders" assetID="USDC-G…" decimals={7} />,
    ],
    [
      '/lending/[pool] — reserve composition',
      <PoolReserves key="pool" pool={POOL} />,
    ],
    [
      '/assets/[slug] — volume by source',
      <SourceBreakdown key="src" asset="USDC-G…" headingLevel={2} />,
    ],
    ['/issuers — directory table', <IssuersTable key="issuers" />],
    ['/sources — source table', <SourcesTable key="sources" />],
    [
      '/dexes/[source] — venue markets table',
      <VenueMarketsTable
        key="venue"
        source="soroswap"
        title="Soroswap pools"
        rowNoun="pools"
        headingLevel={2}
      />,
    ],
  ])('%s', (_name, node) => {
    const { container } = renderInPage(node);
    expectNoSkip(container.innerHTML, 2); // page h1 + the section's own heading
  });

  it('a top-level EmptyState is an h2, and a nested one stays h3', () => {
    // Both directions matter: the fix must not turn every empty state into
    // an h2 (that would break the outline inside a card).
    const top = render(
      <>
        <h1>Pair</h1>
        <EmptyState title="Unrecognised pair" headingLevel={2} />
      </>,
    );
    expect(top.container.querySelector('h2')?.textContent).toBe(
      'Unrecognised pair',
    );
    expectNoSkip(top.container.innerHTML, 2);
    top.unmount();

    const nested = render(<EmptyState title="No rows" />);
    expect(nested.container.querySelector('h3')?.textContent).toBe('No rows');
  });
});
