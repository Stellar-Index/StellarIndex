import { afterEach, describe, expect, it, vi } from 'vitest';

import {
  ROUTES,
  chromeLinkedRoutes,
  reachableRoutes,
  routeFor,
} from '@/lib/route-graph';
import { generateMetadata as assetMetadata } from './assets/[slug]/page';
import { metadata as contractMeta } from './contract/page';
import { metadata as ledgerMeta } from './ledger/page';
import { generateMetadata as pairMetadata } from './markets/[pair]/page';
import { metadata as operationMeta } from './operation/page';
import sitemap from './sitemap';
import { metadata as txMeta } from './tx/page';

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

  /**
   * The sitemap's other half: everything it submits must be a page a
   * READER can also get to.
   *
   * /insights/sponsors and /insights/creators are the shape this is
   * written against. Both return real data, both have finished pages,
   * both were in the sitemap — and the operator still reported "I don't
   * see pages on the explorer for the sponsors / creators", because the
   * only way in is the /insights hub. That case is fine (the hub is in
   * the rail and cards both children), but it is one link away from the
   * failure mode: had /insights lost its rail entry, the pair would have
   * been advertised to Google and unreachable to a person, and nothing
   * would have failed. That is finding S-020 exactly — an orphaned
   * island that links only to itself.
   *
   * A sitemap entry is a crawler hint, never a click path, so the walk
   * (lib/route-graph) deliberately does not treat the sitemap as a root.
   * This test joins the two: sitemap ⊆ reachable-from-the-nav.
   */
  it('submits nothing a reader cannot navigate to', async () => {
    stubFetchOffline();
    const entries = await sitemap();
    const reachable = reachableRoutes();

    const orphaned = [
      ...new Set(
        entries
          .map((e) => new URL(e.url).pathname)
          .map((path) => routeFor(path))
          .filter((route): route is string => route !== null)
          .filter((route) => !reachable.has(route)),
      ),
    ].sort();

    expect(
      orphaned,
      `sitemapped but unreachable from the nav/footer: ${orphaned.join(', ')}`,
    ).toEqual([]);
  });

  /**
   * And the reverse: a sitemap entry that names no page at all. The
   * lending section shipped `/lending/undefined` this way (reading a
   * `contract_id` field the API doesn't send), and the source section
   * emitted ~33 /exchanges|/dexes URLs that 404'd — Google penalises a
   * sitemap full of broken URLs. `routeFor` returning null means no
   * `page.tsx` claims the path.
   *
   * Fetch is stubbed offline here, so only the statically enumerated
   * entries are under test; the API-derived sections fall back to [].
   */
  it('names a real page for every entry', async () => {
    stubFetchOffline();
    const entries = await sitemap();
    const unrouted = [
      ...new Set(
        entries
          .map((e) => new URL(e.url).pathname)
          .filter((path) => routeFor(path) === null),
      ),
    ].sort();

    expect(
      unrouted,
      `sitemap entries with no page.tsx: ${unrouted.join(', ')}`,
    ).toEqual([]);
  });

  /**
   * The third side: a page good enough to put in the nav is good enough
   * to submit. /bridges (linked from search and from the accounts
   * activity panel) and /external/assets (an "External" entry in the
   * rail, whose per-currency CHILDREN were sitemapped while the hub that
   * indexes them was not) were both missing here — the same omission
   * this file already records for the seven chain-explorer hubs, made
   * again by the two newest members of those families.
   *
   * Scoped to what the chrome links directly, because that set is
   * hand-curated: an entry appears there only when someone decided the
   * page is a product surface, which is exactly the decision that should
   * also put it in front of a crawler.
   *
   * STATIC routes only. A dynamic route's sitemap entries come from the
   * API-derived sections (assetPages, convertPages, …), and fetch is
   * stubbed offline here so those correctly fall back to []. Asserting
   * over them would test the stub, not the sitemap.
   */
  const isDynamic = (route: string) => route.includes('[');

  const NOT_FOR_CRAWLERS: ReadonlyMap<string, string> = new Map([
    // robots:noindex — a noindex URL in a sitemap is a Search Console
    // error ("Submitted URL marked 'noindex'"), so these must stay out.
    ['/signin', 'auth gateway, noindex'],
    ['/signup', 'auth gateway, noindex'],
    ['/dashboard', 'authenticated surface, noindex via dashboard/layout'],
    ['/dashboard/admin', 'authenticated surface, noindex via dashboard/layout'],
    ['/dashboard/keys', 'authenticated surface, noindex via dashboard/layout'],
    [
      '/dashboard/price-alerts',
      'authenticated surface, noindex via dashboard/layout',
    ],
    [
      '/dashboard/settings',
      'authenticated surface, noindex via dashboard/layout',
    ],
    ['/dashboard/usage', 'authenticated surface, noindex via dashboard/layout'],
    // Unbounded per-entity long tail, served as a noindex shell.
    ['/accounts/[g]', 'per-account shell, noindex long tail'],
  ]);

  it('submits every page the nav offers', async () => {
    stubFetchOffline();
    const entries = await sitemap();
    const sitemapped = new Set(
      entries
        .map((e) => new URL(e.url).pathname)
        .map((path) => routeFor(path))
        .filter((route): route is string => route !== null),
    );

    const missing = [...chromeLinkedRoutes()]
      .filter((route) => !isDynamic(route))
      .filter((route) => !sitemapped.has(route))
      .filter((route) => !NOT_FOR_CRAWLERS.has(route))
      .sort();

    expect(
      missing,
      `linked from the nav but absent from the sitemap: ${missing.join(', ')}`,
    ).toEqual([]);
  });

  it('keeps the crawler exemptions honest', () => {
    // An exemption for a route that no longer exists is dead weight that
    // would silently cover a future route of the same name.
    const stale = [...NOT_FOR_CRAWLERS.keys()]
      .filter((route) => !ROUTES.has(route))
      .sort();
    expect(
      stale,
      `exempt routes that no longer exist: ${stale.join(', ')}`,
    ).toEqual([]);
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

  /**
   * The query-param entity pages are the third shape of the same defect.
   * /contract?id=, /ledger?seq=, /tx?hash= and /operation?tx=&i= render
   * ENTIRELY from their query string, and each tags itself
   * `canonical: '/<route>'` — so the one URL a crawler can construct, and
   * the one every parameterised hit is consolidated onto, is the bare
   * path, which renders an empty shell.
   *
   * The first three exist specifically to catch inbound legacy links, so
   * they are the most likely of all these pages to actually be crawled.
   * Every canonical counterpart (/contracts/[id], /ledgers/[seq],
   * /transactions/[hash], /accounts/[g]) already carries noindex; these
   * did not.
   *
   * `follow: true` throughout — the outbound links are real and should
   * keep flowing; it is only the empty shell that must not be indexed.
   */
  it('keeps the query-param entity shells out of the index', async () => {
    const shells = {
      '/contract': contractMeta,
      '/ledger': ledgerMeta,
      '/tx': txMeta,
      '/operation': operationMeta,
    };
    for (const [route, meta] of Object.entries(shells)) {
      expect(meta.robots, `${route} is indexable`).toMatchObject({
        index: false,
      });
    }
  });
});
