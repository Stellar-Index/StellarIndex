import { afterEach, describe, expect, it, vi } from 'vitest';

import { generateMetadata as assetMetadata } from './assets/[slug]/page';
import { generateMetadata as pairMetadata } from './markets/[pair]/page';
import sitemap from './sitemap';

/**
 * CRAWL-SURFACE GUARDS
 *
 * The sitemap and the long-tail shells are the two halves of one
 * contract: what the site asks Google to index, and what it asks Google
 * to leave alone. Both halves were inverted at once.
 *
 * The sitemap enumerated the pricing surface but none of the
 * chain-explorer hubs (/network, /ledgers, /transactions, /operations,
 * /accounts, /contracts, /protocols) — indexable, canonical-tagged,
 * content-rich pages reachable only from the nav.
 *
 * Meanwhile /assets/shell/ and /markets/shell/ — the single baked
 * documents functions/{assets,markets}/[[path]].js return with a 200 for
 * EVERY unmatched path under those prefixes — carried indexable
 * metadata, so any garbage URL a crawler tried came back as a soft-404
 * eligible for the index.
 */

// Every API-derived sitemap section catches its own transport failure
// and falls back to []. Rejecting fetch leaves exactly the statically
// enumerated pages, which is the set under test.
function stubFetchOffline() {
  vi.stubGlobal(
    'fetch',
    vi.fn(() => Promise.reject(new Error('offline'))),
  );
}

afterEach(() => {
  vi.unstubAllGlobals();
});

// Hubs that exist on every network (src/lib/network-routes.ts leaves
// chain-native routes ungated), so they belong in every build's sitemap.
const EXPLORER_HUBS = [
  '/network',
  '/ledgers',
  '/transactions',
  '/operations',
  '/accounts',
  '/contracts',
  '/protocols',
];

describe('sitemap', () => {
  it('lists the chain-explorer hubs', async () => {
    stubFetchOffline();
    const entries = await sitemap();
    const paths = new Set(entries.map((e) => new URL(e.url).pathname));
    for (const hub of EXPLORER_HUBS) {
      expect(paths, `sitemap is missing ${hub}`).toContain(`${hub}/`);
    }
  });
});

describe('long-tail shell metadata', () => {
  it('keeps the /assets shell out of the index', async () => {
    // generateStaticParams emits case variants of every slug, so the
    // sentinel reaches the page as shell/SHELL alike.
    for (const slug of ['shell', 'SHELL']) {
      const meta = await assetMetadata({ params: Promise.resolve({ slug }) });
      expect(meta.robots, `/assets/${slug} is indexable`).toMatchObject({
        index: false,
      });
    }
  });

  it('keeps the /markets shell and non-pair slugs out of the index', async () => {
    for (const pair of ['shell', 'not-a-pair']) {
      const meta = await pairMetadata({ params: Promise.resolve({ pair }) });
      expect(meta.robots, `/markets/${pair} is indexable`).toMatchObject({
        index: false,
      });
    }
  });
});
