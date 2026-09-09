// Guard: every route has a click path from the home page.
//
// This file exists because a whole tier of the explorer went dark without
// anything failing. `components/nav/Footer.tsx` was written to carry the
// surfaces the one-screen-tall rail defers — /amm, /yield,
// /liquidity-pools, /convert, /diagnostics, /sla — and the Sidebar's own
// NAV comment promises they "stay reachable via hubs, footer + search".
// Nothing ever rendered the Footer. The component type-checked, linted and
// unit-tested perfectly; the built pages simply had no `<footer>` in them,
// so /yield had zero inbound links, /amm and /liquidity-pools linked only
// to each other, all 361 /convert/{from}/{to} pages were unreachable, and
// /sla had no inbound link anywhere in the repo at all.
//
// None of that is visible to a type check, a render test or an SEO lint —
// each page is individually fine. Only the GRAPH is broken, so the graph is
// what this asserts: start at `/` plus whatever the root layout's own
// component tree links to, follow links page to page, and require that
// every `page.tsx` is reached.
//
// WHY A REACHABILITY WALK AND NOT "does some file mention this href".
// A grep would have passed on the day of the finding: /amm and
// /liquidity-pools each had an inbound link — from each other. An island
// is unreachable however densely it links itself, and only a walk from a
// root sees that.
//
// The sitemap deliberately does NOT count as a root. A sitemap entry is a
// crawler hint, not a click path; a page only listed there is still one no
// reader can navigate to.
//
// Adding a route: give it a link from a page that is already reachable
// (a hub, the footer, or the rail). A route that genuinely has no click
// path — an iframe endpoint, a redirect shim, an email landing page —
// goes in UNLINKED_BY_DESIGN with the reason.
import { join } from 'node:path';

import { describe, expect, it } from 'vitest';

// The walk itself lives in lib/route-graph, shared with
// app/crawl-surface.test.ts so the sitemap guard and this one cannot
// drift into two different notions of "reachable". Only the POLICY —
// which routes are unlinked on purpose — belongs here.
import { ROUTES, chromeRenders, reachableRoutes } from './route-graph';

/**
 * Routes with no click path on purpose, each with the reason. These are
 * real, served pages — they are simply not navigation destinations.
 */
const UNLINKED_BY_DESIGN: ReadonlyMap<string, string> = new Map([
  // Iframe endpoints. /widgets documents them and embeds live examples,
  // but through `src=` on an <iframe>, never an <a href>; they render
  // chrome-free by design (ConsoleShell and Footer both bail on /embed/*).
  ['/embed/asset/[slug]', 'iframe widget endpoint, embedded from /widgets'],
  [
    '/embed/currency/[ticker]',
    'iframe widget endpoint, embedded from /widgets',
  ],
  ['/embed/pair/[pair]', 'iframe widget endpoint, embedded from /widgets'],
  // Legacy query-param entity pages (ADR-0038 Phase D). They exist to
  // catch inbound /contract?id=C… style URLs and redirect to the canonical
  // route; linking to them from the site would be linking to a redirect.
  ['/contract', 'legacy ?id= entry point, redirects to /contracts/{id}'],
  ['/ledger', 'legacy ?seq= entry point, redirects to /ledgers/{seq}'],
  ['/tx', 'legacy ?hash= entry point, redirects to /transactions/{hash}'],
  // Arrived at from a magic-link email, never from inside the site.
  ['/auth/callback', 'magic-link landing; reached from the sign-in email'],
  // The rendered design-system reference (see web/explorer/AGENTS.md).
  // Deliberately not advertised in product navigation.
  ['/dev/primitives', 'design-system reference, not product navigation'],
  ['/dev/styleguide', 'design-system reference, not product navigation'],
]);

describe('route reachability', () => {
  it('finds the route tree', () => {
    // A zero-route walk would report "no dark pages" forever. The explorer
    // has ~82 page.tsx files; anything under 50 means the walk lost the
    // app directory, not that routes were deleted.
    expect(ROUTES.size).toBeGreaterThan(50);
    expect(ROUTES.has('/')).toBe(true);
  });

  it('mounts the footer in the app frame', () => {
    // The direct form of the finding. Everything below depends on the
    // footer's 30-odd links actually rendering, and the failure mode was
    // exactly that the component existed with no importer — which reads,
    // in the reachability output, as a dozen unrelated dark pages.
    expect(chromeRenders(join('components', 'nav', 'Footer.tsx'))).toBe(true);
  });

  it('leaves no page without a click path from the home page', () => {
    const reachable = reachableRoutes();
    const dark = [...ROUTES.keys()]
      .filter((route) => !reachable.has(route))
      .filter((route) => !UNLINKED_BY_DESIGN.has(route))
      .sort();
    expect(dark, `unreachable routes: ${dark.join(', ')}`).toEqual([]);
  });

  it('keeps the by-design exemptions honest', () => {
    // An exemption for a route that no longer exists is dead weight that
    // would silently cover a future route of the same name.
    const stale = [...UNLINKED_BY_DESIGN.keys()]
      .filter((route) => !ROUTES.has(route))
      .sort();
    expect(
      stale,
      `exempt routes that no longer exist: ${stale.join(', ')}`,
    ).toEqual([]);
  });
});
