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

const CONTRACT = /^C[A-Z2-7]{55}$/;

/**
 * The page to send a SAC contract URL to: /v1/assets resolves a classic asset's SAC to that
 * asset, so /assets/{SAC} would be a second page for it. Null when the slug is not a contract or
 * already is the asset's own slug (a Soroban-native token).
 */
export function sacRedirectHref(
  slug: string,
  resolvedSlug: string | null | undefined,
): string | null {
  if (!CONTRACT.test(slug) || !resolvedSlug || resolvedSlug === slug) {
    return null;
  }
  return `/assets/${encodeURIComponent(resolvedSlug)}/`;
}
