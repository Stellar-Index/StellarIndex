/**
 * nav-shell — what a primary-nav page must carry in its STATIC document,
 * as pure functions.
 *
 * Under `output: 'export'` a component that calls `useSearchParams` bails
 * its subtree out to client rendering, and the enclosing Suspense
 * fallback is what the exporter bakes into the HTML. A page written as
 * `<Suspense fallback={null}>{view}</Suspense>` therefore ships a
 * body-less document: no `<h1>`, an empty `<main>`, nothing for a
 * crawler, a no-JS reader, or the first paint. Only useSearchParams
 * CHILDREN belong inside the boundary, and the fallback must be visible
 * content — the page's frame stays outside it.
 *
 * The checks live here, split from the build-output gate that applies
 * them, so the parsing is unit-testable without a `next build`.
 *
 * Deliberately regex-based, like lib/heading-order: the gate runs in
 * `postbuild` against emitted HTML where there is no DOM.
 */

/** The `<main>` element's inner HTML, or null when the page has none. */
function mainInnerHtml(html: string): string | null {
  const m = /<main\b[^>]*>([\s\S]*?)<\/main>/i.exec(html);
  return m ? m[1] : null;
}

/** The static frame a built page shipped inside its `<main>`. */
export type StaticFrame = {
  /** Number of `<h1>` elements inside `<main>`. */
  h1Count: number;
  /** Visible text inside `<main>`, whitespace-collapsed. */
  text: string;
};

/**
 * Read the static frame out of one built page.
 *
 * `<script>` (JSON-LD) and `<template>` are stripped before measuring:
 * a `<template id="B:0">` is React's marker for a Suspense boundary that
 * has NOT resolved, so counting it as content would score exactly the
 * defect this guards as a pass.
 */
export function staticFrame(html: string): StaticFrame | null {
  const inner = mainInnerHtml(html);
  if (inner === null) return null;
  const stripped = inner
    .replace(/<script[\s\S]*?<\/script>/gi, '')
    .replace(/<template[\s\S]*?<\/template>/gi, '');
  return {
    h1Count: (stripped.match(/<h1[\s>]/gi) ?? []).length,
    text: stripped
      .replace(/<[^>]+>/g, ' ')
      .replace(/\s+/g, ' ')
      .trim(),
  };
}

/**
 * The site-relative `href`s declared by a top-level array literal named
 * `constName` in `source`, in declaration order and de-duplicated.
 *
 * The navigation set is parsed from the components that render it
 * rather than duplicated as a list here: a rail entry pointed at a page
 * that never got a static frame is exactly the regression this catches,
 * and a hand-kept copy would not see it. External (https://…) entries
 * are not ours to check.
 */
export function routesFromLiteral(source: string, constName: string): string[] {
  const start = source.indexOf(`const ${constName}`);
  if (start < 0) return [];
  const end = source.indexOf('\n];', start);
  if (end < 0) return [];
  const literal = source.slice(start, end);
  const routes = new Set<string>();
  for (const m of literal.matchAll(/href:\s*'(\/[^']*)'/g)) routes.add(m[1]);
  return [...routes];
}

/** Built-export path for a site-relative route ('/x/y' → 'x/y/index.html'). */
export function exportedHtmlPath(route: string): string {
  return `${route.replace(/^\//, '')}/index.html`;
}
