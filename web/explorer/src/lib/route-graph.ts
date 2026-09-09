/**
 * route-graph — the explorer's link graph, as pure functions over the
 * `src/app` tree.
 *
 * Two guards need the same walk and must not each grow their own copy:
 *
 *   lib/route-reachability.test.ts — every `page.tsx` has a click path
 *     from `/`, so no page goes dark the way /yield, /amm and /sla did.
 *   app/crawl-surface.test.ts — everything the sitemap submits to a
 *     crawler is a page a *reader* can also navigate to, so an orphaned
 *     island can't be indexable and unreachable at once.
 *
 * The mechanics live here; the POLICY (which routes are unlinked on
 * purpose, what the sitemap must contain) stays in the tests that own it.
 *
 * Test-only, like lib/nav-shell: it reads the source tree with node:fs,
 * so it must never be imported from application code.
 *
 * WHY A REACHABILITY WALK AND NOT "does some file mention this href".
 * A grep passes on an island: /amm and /liquidity-pools each had an
 * inbound link — from each other. An island is unreachable however
 * densely it links itself, and only a walk from a root sees that.
 */
import { existsSync, readdirSync, readFileSync, statSync } from 'node:fs';
import { dirname, join, relative, resolve } from 'node:path';

const SRC = join(__dirname, '..');
const APP = join(SRC, 'app');

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

/** Route → the `page.tsx` that serves it. */
export const ROUTES: ReadonlyMap<string, string> = (() => {
  const routes = new Map<string, string>();
  for (const file of walk(APP)) {
    if (/(?:^|\/)page\.tsx$/.test(file)) routes.set(routeOf(file), file);
  }
  return routes;
})();

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

const STATIC_IMPORT =
  /(?:^|\n)\s*(?:import|export)[\s\S]*?from\s*['"]([^'"]+)['"]/g;
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
export function moduleClosure(entries: readonly string[]): Set<string> {
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
const NAVIGATE =
  /\b(?:push|replace|redirect|permanentRedirect)\(\s*(['"`])(\/[^'"`\n]*)\1/g;
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
export function routeFor(path: string): string | null {
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
 * The root layout's module closure — the nav every page renders (rail,
 * footer, search modal). Its links are reachable from anywhere.
 */
export function globalChrome(): Set<string> {
  return moduleClosure([join(APP, 'layout.tsx')]);
}

/** Whether the root layout actually renders the given component file. */
export function chromeRenders(basename: string): boolean {
  return [...globalChrome()].some((file) => file.endsWith(basename));
}

/**
 * The routes the global chrome links DIRECTLY — the nav rail, the footer
 * and the search modal. These are the curated, hand-maintained product
 * destinations, as opposed to the much larger set reachable by following
 * links onward from them.
 */
export function chromeLinkedRoutes(): Set<string> {
  return routesLinkedFrom(globalChrome());
}

/**
 * Breadth-first walk of the route graph from the site root.
 *
 * Seeded with `/` and everything the global chrome links — i.e. the nav
 * rail, the footer and the search modal — then followed page to page. A
 * route in the returned set is one a reader can actually click to.
 *
 * The chrome's own modules are excluded from each page's closure so a
 * page doesn't inherit credit for links the chrome makes.
 *
 * The sitemap deliberately is NOT a root. A sitemap entry is a crawler
 * hint, not a click path; a page only listed there is still one no
 * reader can navigate to.
 */
export function reachableRoutes(): Set<string> {
  const chrome = globalChrome();
  const reachable = new Set<string>(['/']);
  for (const route of routesLinkedFrom(chrome)) reachable.add(route);

  const queue = [...reachable];
  while (queue.length > 0) {
    const route = queue.shift() as string;
    const pageFile = ROUTES.get(route);
    if (pageFile === undefined) continue;
    const own = [...moduleClosure(routeEntries(pageFile))].filter(
      (file) => !chrome.has(file),
    );
    for (const next of routesLinkedFrom(own)) {
      if (reachable.has(next)) continue;
      reachable.add(next);
      queue.push(next);
    }
  }
  return reachable;
}
