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
import { existsSync, readdirSync, readFileSync, statSync } from 'node:fs';
import { dirname, join, relative, resolve } from 'node:path';

import { describe, expect, it } from 'vitest';

const SRC = join(__dirname, '..');
const APP = join(SRC, 'app');

/**
 * Routes with no click path on purpose, each with the reason. These are
 * real, served pages — they are simply not navigation destinations.
 */
const UNLINKED_BY_DESIGN: ReadonlyMap<string, string> = new Map([
  // Iframe endpoints. /widgets documents them and embeds live examples,
  // but through `src=` on an <iframe>, never an <a href>; they render
  // chrome-free by design (ConsoleShell and Footer both bail on /embed/*).
  ['/embed/asset/[slug]', 'iframe widget endpoint, embedded from /widgets'],
  ['/embed/currency/[ticker]', 'iframe widget endpoint, embedded from /widgets'],
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

/** Every file under a directory, recursively. */
function walk(dir: string, out: string[] = []): string[] {
  for (const entry of readdirSync(dir)) {
    const full = join(dir, entry);
    if (statSync(full).isDirectory()) walk(full, out);
    else out.push(full);
  }
  return out;
}

/**
 * The route a `page.tsx` serves: its directory relative to app/, with
 * route groups — `(marketing)` — dropped, since they shape the layout
 * tree and not the URL.
 */
function routeOf(pageFile: string): string {
  const rel = relative(APP, dirname(pageFile));
  const segments =
    rel === '' ? [] : rel.split('/').filter((s) => !/^\(.*\)$/.test(s));
  return `/${segments.join('/')}`;
}

const ROUTES = new Map<string, string>();
for (const file of walk(APP)) {
  if (/(?:^|\/)page\.tsx$/.test(file)) ROUTES.set(routeOf(file), file);
}

/**
 * Strip `//` line comments and block comments. Prose names paths
 * constantly ("see /liquidity-pools") and a comment is not a link — the
 * whole point of the finding is that a page can be talked about
 * everywhere and linked from nowhere.
 *
 * Line comments first: a `/*` inside a `//` line would otherwise open a
 * phantom block comment that swallows real code (same trap documented in
 * network-hardcodes.test.ts).
 */
function stripComments(src: string): string {
  return src.replace(/^\s*\/\/.*$/gm, '').replace(/\/\*[\s\S]*?\*\//g, '');
}

const sourceCache = new Map<string, string>();
function sourceOf(file: string): string {
  let src = sourceCache.get(file);
  if (src === undefined) {
    src = stripComments(readFileSync(file, 'utf8'));
    sourceCache.set(file, src);
  }
  return src;
}

const MODULE_SUFFIXES = ['', '.tsx', '.ts', '/index.tsx', '/index.ts'];

/** Resolve an `@/…` or relative import to a file in src/, or null. */
function resolveImport(spec: string, fromFile: string): string | null {
  let base: string;
  if (spec.startsWith('@/')) base = join(SRC, spec.slice(2));
  else if (spec.startsWith('./') || spec.startsWith('../'))
    base = resolve(dirname(fromFile), spec);
  else return null; // bare package
  for (const suffix of MODULE_SUFFIXES) {
    const candidate = base + suffix;
    if (existsSync(candidate) && statSync(candidate).isFile()) return candidate;
  }
  return null;
}

const STATIC_IMPORT = /(?:^|\n)\s*(?:import|export)[\s\S]*?from\s*['"]([^'"]+)['"]/g;
const DYNAMIC_IMPORT = /import\(\s*['"]([^'"]+)['"]\s*\)/g;

const importCache = new Map<string, string[]>();
function importsOf(file: string): string[] {
  let found = importCache.get(file);
  if (found === undefined) {
    const src = sourceOf(file);
    found = [];
    for (const pattern of [STATIC_IMPORT, DYNAMIC_IMPORT]) {
      pattern.lastIndex = 0;
      let match: RegExpExecArray | null;
      while ((match = pattern.exec(src)) !== null) {
        const resolved = resolveImport(match[1], file);
        if (resolved) found.push(resolved);
      }
    }
    importCache.set(file, found);
  }
  return found;
}

/**
 * Every module a set of entry files pulls in, transitively. A page's links
 * mostly live in the components it renders, not in `page.tsx` — the rail's
 * hrefs are in Sidebar.tsx, three imports below app/layout.tsx.
 */
function moduleClosure(entries: readonly string[]): Set<string> {
  const seen = new Set<string>();
  const stack = [...entries];
  while (stack.length > 0) {
    const file = stack.pop();
    if (file === undefined || seen.has(file)) continue;
    if (/\.test\.tsx?$/.test(file)) continue; // a test is not a link
    seen.add(file);
    for (const next of importsOf(file)) stack.push(next);
  }
  return seen;
}

// A link is an `href` (JSX attribute or nav-item property), a programmatic
// navigation, or a path-building helper's return. `${…}` collapses to `*`
// so `/assets/${slug}` matches the /assets/[slug] route.
const HREF = /\bhref\s*[=:]\s*\{?\s*(['"`])(\/[^'"`\n]*)\1/g;
const NAVIGATE = /\b(?:push|replace|redirect|permanentRedirect)\(\s*(['"`])(\/[^'"`\n]*)\1/g;
const RETURNED_PATH = /\breturn\s+(['"`])(\/[^'"`\n]*)\1/g;

function linkedPaths(file: string): Set<string> {
  const src = sourceOf(file);
  const paths = new Set<string>();
  for (const pattern of [HREF, NAVIGATE, RETURNED_PATH]) {
    pattern.lastIndex = 0;
    let match: RegExpExecArray | null;
    while ((match = pattern.exec(src)) !== null) {
      const raw = match[2];
      if (raw.startsWith('//')) continue; // protocol-relative, i.e. external
      paths.add(raw.replace(/\$\{[^}]*\}/g, '*'));
    }
  }
  return paths;
}

/**
 * The route a linked path lands on, or null when it names no route (an
 * API path, a static asset, a feed). Literal segments beat dynamic ones,
 * so `/assets/verified` prefers a literal route over /assets/[slug].
 */
function routeFor(path: string): string | null {
  const clean = path.split('?')[0].split('#')[0].replace(/\/+$/, '') || '/';
  const segments = clean === '/' ? [] : clean.slice(1).split('/');
  let best: { route: string; literals: number } | null = null;
  for (const route of ROUTES.keys()) {
    const pattern = route === '/' ? [] : route.slice(1).split('/');
    if (pattern.length !== segments.length) continue;
    let literals = 0;
    let matches = true;
    for (let i = 0; i < pattern.length; i++) {
      if (pattern[i].startsWith('[')) {
        if (!segments[i]) {
          matches = false;
          break;
        }
      } else if (pattern[i] === segments[i]) literals++;
      else {
        matches = false;
        break;
      }
    }
    if (matches && (best === null || literals > best.literals))
      best = { route, literals };
  }
  return best === null ? null : best.route;
}

function routesLinkedFrom(modules: Iterable<string>): Set<string> {
  const linked = new Set<string>();
  for (const file of modules) {
    for (const path of linkedPaths(file)) {
      const route = routeFor(path);
      if (route !== null) linked.add(route);
    }
  }
  return linked;
}

/** page.tsx plus every layout.tsx above it — the chrome a route renders. */
function routeEntries(pageFile: string): string[] {
  const entries = [pageFile];
  let dir = dirname(pageFile);
  for (;;) {
    const layout = join(dir, 'layout.tsx');
    if (existsSync(layout)) entries.push(layout);
    if (dir === APP) break;
    dir = dirname(dir);
  }
  return entries;
}

/**
 * Breadth-first walk of the route graph from the site root.
 *
 * `globalChrome` is the root layout's module closure — the nav that every
 * page renders. Its links are reachable from anywhere, and its modules are
 * excluded from each page's own closure so a page doesn't inherit credit
 * for links the chrome makes.
 */
function reachableRoutes(): Set<string> {
  const globalChrome = moduleClosure([join(APP, 'layout.tsx')]);
  const reachable = new Set<string>(['/']);
  for (const route of routesLinkedFrom(globalChrome)) reachable.add(route);

  const queue = [...reachable];
  while (queue.length > 0) {
    const route = queue.shift() as string;
    const pageFile = ROUTES.get(route);
    if (pageFile === undefined) continue;
    const own = [...moduleClosure(routeEntries(pageFile))].filter(
      (file) => !globalChrome.has(file),
    );
    for (const next of routesLinkedFrom(own)) {
      if (reachable.has(next)) continue;
      reachable.add(next);
      queue.push(next);
    }
  }
  return reachable;
}

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
    const chrome = moduleClosure([join(APP, 'layout.tsx')]);
    const mounted = [...chrome].some((file) =>
      file.endsWith(join('components', 'nav', 'Footer.tsx')),
    );
    expect(mounted).toBe(true);
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
    expect(stale, `exempt routes that no longer exist: ${stale.join(', ')}`)
      .toEqual([]);
  });
});
