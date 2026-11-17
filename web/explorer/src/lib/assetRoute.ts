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
  resolved:
    | { slug?: string | null; code?: string | null; asset_id?: string | null }
    | null
    | undefined,
): string | null {
  if (!CONTRACT.test(slug) || !resolved) return null;
  // The server fills a missing slug with the bare code, which routes to whichever asset owns that
  // ticker; a look-alike's SAC must land on its own (code, issuer) page.
  const target =
    resolved.slug && resolved.slug !== resolved.code
      ? resolved.slug
      : resolved.asset_id;
  if (!target || target === slug) return null;
  return `/assets/${encodeURIComponent(target)}/`;
}
