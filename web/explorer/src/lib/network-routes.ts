// network-routes — the ONE table saying which explorer routes have data
// on which network, and the one predicate every nav / discovery surface
// asks before offering a link.
//
// Why this exists (#328). The answer used to be duplicated in four
// places that had drifted apart: the Sidebar's TESTNET_HIDDEN_HREFS, the
// Footer's LEAN_HIDDEN_HREFS, the SearchModal's static page list, and
// the sitemap. So /anomalies, /divergences and /mev — all
// aggregator-derived and structurally empty on the lean test nets — were
// dropped from the rail (via their /insights hub) yet still offered by
// site search, still listed in the test net's sitemap, and still carded
// on the /network hub. When the rule lives inside a component, every NEW
// entry point is ungated BY DEFAULT, which is exactly how those three
// pages survived two rounds of test-net gating.
//
// Adding a route: declare the capability it needs below. Routes absent
// from the table are available everywhere — that is the right default
// for chain-native surfaces (/ledgers, /transactions, /accounts,
// /contracts, /network), which have real data on every network.
import { CURRENT_NETWORK, type NetworkInfo } from './networks';

/**
 * The network capability a route's data depends on.
 *
 *   pricing  — needs the aggregator's USD price/volume layer. Mainnet
 *              only; the lean test nets run no aggregator, so every
 *              figure on these pages is null.
 *   accounts — needs the API-key/SaaS backend (mainnet only).
 *   assets   — needs issued assets to exist on-chain.
 *   sdex     — needs classic order-book / liquidity-pool activity.
 *   bridges  — needs cross-chain bridge deployments (CCTP, Rozo),
 *              which are pubnet-only contracts.
 */
export type NetworkCapability =
  'pricing' | 'accounts' | 'assets' | 'sdex' | 'bridges';

/**
 * Route → the capability it requires. Keys are matched against the
 * longest leading path prefix of an href, so '/convert' covers
 * '/convert/USD/EUR' and '/exchanges' covers '/exchanges/kraken'.
 */
const ROUTE_CAPABILITY: ReadonlyMap<string, NetworkCapability> = new Map([
  // ── Aggregator / USD-pricing derived ──
  ['/markets', 'pricing'],
  ['/exchanges', 'pricing'],
  ['/external/assets', 'pricing'],
  ['/lending', 'pricing'],
  ['/aggregators', 'pricing'],
  ['/oracles', 'pricing'],
  ['/amm', 'pricing'],
  ['/yield', 'pricing'],
  ['/convert', 'pricing'],
  // The insights hub and the three feeds it hosts. All three are
  // aggregator outputs (cross-venue divergence, VWAP anomalies, arb
  // cycles priced in USD) and return empty on a net with no aggregator.
  ['/insights', 'pricing'],
  ['/anomalies', 'pricing'],
  ['/divergences', 'pricing'],
  ['/mev', 'pricing'],
  // ── Customer accounts / API-key SaaS ──
  ['/signin', 'accounts'],
  ['/signup', 'accounts'],
  ['/dashboard', 'accounts'],
  // ── Chain-shape dependent ──
  ['/assets', 'assets'],
  ['/issuers', 'assets'],
  ['/sdex', 'sdex'],
  ['/liquidity-pools', 'sdex'],
  ['/bridges', 'bridges'],
]);

function hasCapability(net: NetworkInfo, cap: NetworkCapability): boolean {
  switch (cap) {
    case 'pricing':
      return net.pricing;
    case 'accounts':
      return net.accounts;
    case 'assets':
      return net.hasAssets;
    case 'sdex':
      return net.hasSdexActivity;
    case 'bridges':
      return net.hasBridges;
  }
}

/**
 * The capability `href` requires on this network, or null when the route
 * is network-agnostic. Matches the longest declared prefix so nested
 * routes inherit their hub's requirement ('/assets/USDC' → 'assets').
 */
export function routeCapability(href: string): NetworkCapability | null {
  // External links (https://docs.…) and anchors are never gated.
  if (!href.startsWith('/')) return null;
  const path = href.split('?')[0].split('#')[0];
  let best: NetworkCapability | null = null;
  let bestLen = 0;
  for (const [prefix, cap] of ROUTE_CAPABILITY) {
    if (prefix.length <= bestLen) continue;
    if (path === prefix || path.startsWith(`${prefix}/`)) {
      best = cap;
      bestLen = prefix.length;
    }
  }
  return best;
}

/**
 * Whether `href` has data on `net` (this build's network by default).
 * Nav, search, sitemap and card grids all ask this — a link that fails
 * it must not be offered, because the page behind it is empty by
 * construction rather than merely quiet.
 */
export function routeAvailable(
  href: string,
  net: NetworkInfo = CURRENT_NETWORK,
): boolean {
  const cap = routeCapability(href);
  return cap === null || hasCapability(net, cap);
}

/** Filter any list of `{ href }` link descriptors through routeAvailable. */
export function availableRoutes<T extends { href: string }>(
  items: readonly T[],
  net: NetworkInfo = CURRENT_NETWORK,
): T[] {
  return items.filter((it) => routeAvailable(it.href, net));
}
