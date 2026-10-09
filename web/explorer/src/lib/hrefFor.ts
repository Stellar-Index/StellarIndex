// hrefFor — the routing chokepoint for API-supplied identifiers that are
// NOT constrained to a URL-safe charset by the OpenAPI schema (source
// names, protocol slugs, incident filenames). Route every such link
// through here instead of a hand-built template literal: an operator
// name containing `/`, `?`, `#` or `%` (an oracle registered as
// `band/v2`, a protocol slug with a space) breaks the link silently
// otherwise.
export const hrefFor = {
  source: (name: string): string => `/sources/${encodeURIComponent(name)}`,
  exchange: (name: string): string => `/exchanges/${encodeURIComponent(name)}`,
  // SDEX's one page is /sdex; /dexes/sdex 301s there.
  dex: (source: string): string =>
    source === 'sdex' ? '/sdex' : `/dexes/${encodeURIComponent(source)}`,
  protocol: (slug: string): string => `/protocols/${encodeURIComponent(slug)}`,
  // Trailing slash matches StatusPageClient's existing incident links.
  incident: (slug: string): string =>
    `/status/incident/${encodeURIComponent(slug)}/`,
};
