import type { Metadata } from 'next';
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
import { generateMetadata as issuerMetadata } from './issuers/[g_strkey]/page';
import { generateMetadata as poolMetadata } from './lending/[pool]/page';
import { generateMetadata as pairMetadata } from './markets/[pair]/page';
import { metadata as operationMeta } from './operation/page';
import sitemap from './sitemap';
import { metadata as txMeta } from './tx/page';

/**
 * CRAWL-SURFACE GUARDS
 *
 * The sitemap and the long-tail shells are two halves of one contract:
 * what the site asks Google to index, and what it asks Google to leave
 * alone.
 */

// Every API-derived sitemap section goes through buildFetch, which fails the
// build on an unreachable or empty listing. Stub one benign row per listing
// so these tests, which assert on the STATICALLY enumerated pages, don't
// depend on live API reachability.
function stubFetchMinimal() {
  const ok = (rows: unknown) =>
    Promise.resolve(
      new Response(JSON.stringify({ data: rows }), {
        status: 200,
        headers: { 'content-type': 'application/json' },
      }),
    );
  vi.stubGlobal(
    'fetch',
    vi.fn((input: RequestInfo | URL) => {
      const url = String(input);
      if (url.includes('/v1/sources')) {
        return ok([{ name: 'sdex', class: 'exchange', subclass: 'dex' }]);
      }
      if (url.includes('/v1/lending/pools')) return ok([]);
      if (url.includes('/v1/markets')) {
        return ok([{ base: 'native', quote: 'USDC-ISSUER' }]);
      }
      if (url.includes('/v1/assets/verified')) {
        return ok([{ ticker: 'USD', class: 'fiat' }]);
      }
      if (url.includes('/v1/issuers')) {
        // gitleaks:allow — fake fixture shape, not a real Stellar account
        return ok([
          { g_strkey: 'GATESTSITEMAPXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXX' },
        ]);
      }
      if (url.includes('/v1/assets')) return ok([{ slug: 'xlm' }]);
      return ok([]);
    }),
  );
}

afterEach(() => {
  vi.unstubAllGlobals();
});

async function sitemapPaths() {
  stubFetchMinimal();
  return (await sitemap()).map((e) => new URL(e.url).pathname);
}

// Chain-native hubs exist on every network, so every build's sitemap lists them.
const EXPLORER_HUBS = [
  '/network',
  '/ledgers',
  '/transactions',
  '/operations',
  '/accounts',
  '/contracts',
  '/protocols',
];

const AUTH = 'authenticated surface, noindex via dashboard/layout';
// A noindex URL in a sitemap is a Search Console error, so these stay out.
const NOT_FOR_CRAWLERS: ReadonlyMap<string, string> = new Map([
  ['/signin', 'auth gateway, noindex'],
  ['/signup', 'auth gateway, noindex'],
  ...[
    '/dashboard',
    '/dashboard/admin',
    '/dashboard/keys',
    '/dashboard/price-alerts',
    '/dashboard/settings',
    '/dashboard/usage',
    '/dashboard/webhooks',
  ].map((r): [string, string] => [r, AUTH]),
  ['/accounts/[g]', 'per-account shell, noindex long tail'],
]);

describe('sitemap', () => {
  it('lists the chain-explorer hubs', async () => {
    const paths = new Set(await sitemapPaths());
    for (const hub of EXPLORER_HUBS) {
      expect(paths, `sitemap is missing ${hub}`).toContain(`${hub}/`);
    }
  });

  // sitemap is a crawler hint, never a click path, so the route-graph walk
  // does not root at it; this joins the two: sitemap ⊆ reachable-from-nav.
  it('submits nothing a reader cannot navigate to', async () => {
    const reachable = reachableRoutes();
    const orphaned = [
      ...new Set(
        (await sitemapPaths())
          .map((path) => routeFor(path))
          .filter((route): route is string => route !== null)
          .filter((route) => !reachable.has(route)),
      ),
    ].sort();
    expect(
      orphaned,
      `sitemapped but unreachable from the nav/footer: ${orphaned.join(', ')}`,
    ).toEqual([]);
  }, 15_000); // walks the whole route graph and builds the sitemap; 5 s flakes under verify's parallel lanes

  // routeFor returning null means no page.tsx claims the path.
  it('names a real page for every entry', async () => {
    const unrouted = [
      ...new Set((await sitemapPaths()).filter((p) => routeFor(p) === null)),
    ].sort();
    expect(
      unrouted,
      `sitemap entries with no page.tsx: ${unrouted.join(', ')}`,
    ).toEqual([]);
  });

  // STATIC routes only: dynamic routes' entries come from API-derived
  // sections, which the offline stub cannot meaningfully exercise.
  it('submits every page the nav offers', async () => {
    const sitemapped = new Set(
      (await sitemapPaths())
        .map((path) => routeFor(path))
        .filter((route): route is string => route !== null),
    );
    const missing = [...chromeLinkedRoutes()]
      .filter((route) => !route.includes('['))
      .filter((route) => !sitemapped.has(route))
      .filter((route) => !NOT_FOR_CRAWLERS.has(route))
      .sort();
    expect(
      missing,
      `linked from the nav but absent from the sitemap: ${missing.join(', ')}`,
    ).toEqual([]);
  });

  it('keeps the crawler exemptions honest', () => {
    // An exemption for a vanished route would silently cover a future route of the same name.
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
  const shells: [string, () => Promise<Metadata>][] = [
    [
      '/assets/shell',
      () => assetMetadata({ params: Promise.resolve({ slug: 'shell' }) }),
    ],
    // generateStaticParams emits case variants of every slug.
    [
      '/assets/SHELL',
      () => assetMetadata({ params: Promise.resolve({ slug: 'SHELL' }) }),
    ],
    [
      '/markets/shell',
      () => pairMetadata({ params: Promise.resolve({ pair: 'shell' }) }),
    ],
    [
      '/markets/not-a-pair',
      () => pairMetadata({ params: Promise.resolve({ pair: 'not-a-pair' }) }),
    ],
    [
      '/issuers/shell',
      () => issuerMetadata({ params: Promise.resolve({ g_strkey: 'shell' }) }),
    ],
    [
      '/lending/shell',
      () => poolMetadata({ params: Promise.resolve({ pool: 'shell' }) }),
    ],
  ];

  it.each(shells)('keeps %s out of the index', async (route, load) => {
    expect((await load()).robots, `${route} is indexable`).toMatchObject({
      index: false,
    });
  });

  // Next's metadata merge only touches keys present on the returned object, so
  // a shell with no `alternates` key inherits the root layout's canonical '/'.
  it.each(
    shells.filter(
      ([route]) => route !== '/assets/SHELL' && route !== '/markets/not-a-pair',
    ),
  )(
    '%s declares an empty alternates instead of inheriting the root canonical',
    async (route, load) => {
      const meta = await load();
      expect(
        meta,
        `${route} must declare alternates itself, not inherit the root layout canonical`,
      ).toHaveProperty('alternates');
      expect(meta.alternates?.canonical).toBeUndefined();
    },
  );

  // These render entirely from their query string and tag themselves with a
  // bare-path canonical, so the one crawlable URL is an empty shell.
  it.each([
    ['/contract', contractMeta],
    ['/ledger', ledgerMeta],
    ['/tx', txMeta],
    ['/operation', operationMeta],
  ])('keeps the query-param shell %s out of the index', (route, meta) => {
    expect(meta.robots, `${route} is indexable`).toMatchObject({
      index: false,
    });
  });
});
