// Off-chain reference ids (crypto:BTC, fiat:USD) and oracle-only raw symbols
// have no /assets/[slug] page; the explorer pages that render them are below.
export function offChainAssetHref(slug: string): string | null {
  const m = /^(crypto|fiat|raw):(.+)$/i.exec(slug);
  if (!m) return null;
  const kind = m[1]!.toLowerCase();
  if (kind === 'raw') return '/oracles/';
  // crypto:XLM is an alias of native; the external page has no XLM row.
  if (kind === 'crypto' && m[2]!.toUpperCase() === 'XLM')
    return '/assets/native/';
  return `/external/assets/${encodeURIComponent(m[2]!.toLowerCase())}/`;
}
