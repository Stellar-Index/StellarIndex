// Slug normalisation for /convert/{from}/{to}. The baked pages are keyed by
// upper-case ticker (XLM or an ISO 4217 fiat code).
const ALIASES = new Map([
  ['native', 'XLM'],
  ['crypto:xlm', 'XLM'],
]);

/** The canonical ticker for a /convert path segment, or null if it names none. */
export function canonicalConvertTicker(segment) {
  let raw;
  try {
    raw = decodeURIComponent(segment).trim().toLowerCase();
  } catch {
    return null;
  }
  const alias = ALIASES.get(raw);
  if (alias) return alias;
  const bare = raw.startsWith('fiat:') ? raw.slice('fiat:'.length) : raw;
  // A code-issuer id (USDC-GA5Z…) must never collapse to its bare code.
  return /^[a-z]{3,5}$/.test(bare) ? bare.toUpperCase() : null;
}

/** {from, to} parsed from a /convert/{from}/{to}[/] path, or null. */
export function parseConvertPath(pathname) {
  const m = pathname.match(/^\/convert\/([^/]+)\/([^/]+)\/?$/);
  if (!m) return null;
  const from = canonicalConvertTicker(m[1]);
  const to = canonicalConvertTicker(m[2]);
  if (!from || !to || from === to) return null;
  return { from, to, canonical: `/convert/${from}/${to}/` };
}
