// networks — the registry of StellarIndex explorer deployments, one per
// Stellar network. Each network has its OWN explorer origin and its OWN API
// origin (api.* is DNS-only so its SSE streams aren't buffered by a proxy).
//
// This drives the nav network-switcher: the current deployment identifies
// itself via NEXT_PUBLIC_NETWORK (set at build time — mainnet is the default),
// and the switcher lists the other networks with their live tip so a visitor
// can hop between explorers.

export type NetworkId = 'mainnet' | 'testnet' | 'futurenet';

export interface NetworkInfo {
  id: NetworkId;
  /** Full label for the dropdown row, e.g. "Mainnet". */
  label: string;
  /** Lower-case short tag shown inline next to the odometer, e.g. "mainnet". */
  tag: string;
  /**
   * The network's name in STELLAR's own vernacular, for surfaces that
   * identify the chain rather than this deployment — mainnet is "Pubnet"
   * to Stellar, not "Mainnet". Kept distinct from `label` (which names the
   * deployment in our switcher UI) so neither can be changed for the
   * other's sake.
   */
  stellarName: string;
  /**
   * This network's path segment on stellar.expert, or null where
   * stellar.expert has no explorer for it. Every outbound stellar.expert
   * link was hardcoded to `public` (mainnet), so on a test net a "view on
   * stellar.expert" link sent the reader to MAINNET and showed them either
   * nothing or, worse, an unrelated mainnet entity with a colliding id.
   * null => render no link at all rather than a knowingly-wrong one.
   */
  stellarExpertPath: string | null;
  /**
   * This network's stellarchain.io origin. Unlike stellar.expert,
   * stellarchain.io hosts all three networks (verified 2026-08-27), so this
   * is never null — which is why it is the one cross-reference that still
   * works on futurenet.
   */
  stellarChainUrl: string;
  /** This network's explorer origin (where the switcher link points). */
  explorerUrl: string;
  /** This network's API origin (grey/DNS-only) for the live-tip probe. */
  apiBaseUrl: string;
  /**
   * Whether this network's explorer is deployed and reachable. Futurenet is
   * Phase 2 — listed (so the design is visible) but flagged not-yet-live so
   * the switcher shows it disabled instead of linking to a dead origin.
   */
  live: boolean;
  /**
   * Whether this network has aggregator-derived USD pricing. Mainnet only —
   * the lean test nets run no aggregator (every asset is $0), so pricing
   * widgets and the /v1/price/tip/stream are skipped there (an always-404
   * stream would just retry forever).
   */
  pricing: boolean;
  /**
   * Whether this network runs the accounts/API-key SaaS backend. Mainnet
   * only — the lean test nets are free public explorers with no customer
   * accounts, so /v1/account/me 503s there. When false the explorer skips
   * the credentialed session probe and hides the sign-in / dashboard surfaces
   * entirely (an always-503 probe would log an error on every page load).
   */
  accounts: boolean;
  /**
   * Whether this network has issued assets worth a browse surface.
   * False on futurenet, which is a contracts-only protocol-preview
   * chain: measured 2026-08-27 it carries 0 issued assets, so /assets,
   * /issuers and the home "Top assets" grid are structurally empty
   * there. Before #328 futurenet was special-cased by id on the
   * homepage ONLY, so the nav still offered the empty pages.
   */
  hasAssets: boolean;
  /**
   * Whether this network has SDEX (classic order-book) trading worth a
   * surface. False on futurenet for the same reason as [hasAssets] —
   * 0 SDEX trades — which makes /sdex and /liquidity-pools empty
   * rather than merely unpriced.
   */
  hasSdexActivity: boolean;
  /**
   * Whether this network has cross-chain bridge deployments (Circle
   * CCTP, Rozo) worth a /bridges surface. Mainnet only — both are
   * pubnet-only contract deployments, so /bridges renders an empty
   * roster on the test nets. (This was already the SearchModal's
   * shipped assertion before #328 moved it into the shared table.)
   */
  hasBridges: boolean;
}

// Ordered mainnet → testnet → futurenet (production first, protocol-upgrade
// pipeline order thereafter).
export const NETWORKS: NetworkInfo[] = [
  {
    id: 'mainnet',
    label: 'Mainnet',
    tag: 'mainnet',
    stellarName: 'Pubnet',
    stellarExpertPath: 'public',
    stellarChainUrl: 'https://stellarchain.io',
    explorerUrl: 'https://stellarindex.io',
    apiBaseUrl: 'https://api.stellarindex.io',
    live: true,
    pricing: true,
    accounts: true,
    hasAssets: true,
    hasSdexActivity: true,
    hasBridges: true,
  },
  {
    id: 'testnet',
    label: 'Testnet',
    tag: 'testnet',
    stellarName: 'Testnet',
    stellarExpertPath: 'testnet',
    stellarChainUrl: 'https://testnet.stellarchain.io',
    explorerUrl: 'https://testnet.stellarindex.io',
    apiBaseUrl: 'https://api.testnet.stellarindex.io',
    live: true,
    pricing: false,
    accounts: false,
    hasAssets: true,
    hasSdexActivity: true,
    hasBridges: false,
  },
  {
    id: 'futurenet',
    label: 'Futurenet',
    tag: 'futurenet',
    stellarName: 'Futurenet',
    stellarExpertPath: null,
    stellarChainUrl: 'https://futurenet.stellarchain.io',
    explorerUrl: 'https://futurenet.stellarindex.io',
    apiBaseUrl: 'https://api.futurenet.stellarindex.io',
    live: true,
    pricing: false,
    accounts: false,
    // Contracts-only preview chain — 0 issued assets, 0 SDEX trades
    // (measured 2026-08-27). See the flags' docs on NetworkInfo.
    hasAssets: false,
    hasSdexActivity: false,
    hasBridges: false,
  },
];

const NETWORKS_BY_ID: Record<NetworkId, NetworkInfo> = Object.fromEntries(
  NETWORKS.map((n) => [n.id, n]),
) as Record<NetworkId, NetworkInfo>;

/**
 * The network THIS explorer build serves. Set NEXT_PUBLIC_NETWORK at build
 * time (mainnet | testnet | futurenet); defaults to mainnet so the primary
 * deployment needs no extra env.
 */
export const CURRENT_NETWORK_ID: NetworkId = ((): NetworkId => {
  const raw = process.env.NEXT_PUBLIC_NETWORK;
  if (raw === 'testnet' || raw === 'futurenet' || raw === 'mainnet') return raw;
  return 'mainnet';
})();

export const CURRENT_NETWORK: NetworkInfo = NETWORKS_BY_ID[CURRENT_NETWORK_ID];

/** The networks OTHER than the one this explorer serves (for the switcher). */
export const OTHER_NETWORKS: NetworkInfo[] = NETWORKS.filter(
  (n) => n.id !== CURRENT_NETWORK_ID,
);

/**
 * Absolute stellar.expert URL for an entity on THIS network, or null when
 * stellar.expert has no explorer for it (futurenet). Callers must handle
 * null by omitting the link — a link to another network's explorer is worse
 * than no link, because the id may resolve there to something unrelated.
 *
 * `kind` is stellar.expert's own segment: 'tx' | 'account' | 'contract' | 'asset'.
 */
export function stellarExpertUrl(kind: string, id: string): string | null {
  const net = CURRENT_NETWORK.stellarExpertPath;
  if (!net) return null;
  return `https://stellar.expert/explorer/${net}/${kind}/${encodeURIComponent(id)}`;
}

/**
 * Absolute stellarchain.io URL for an entity on THIS network.
 *
 * Path segments verified live 2026-08-27 by response size — the PLURAL forms
 * (`/transactions/`, `/accounts/`, `/contracts/`) return a server-rendered
 * page (~40 KB), while the singular forms return the same ~20 KB empty SPA
 * shell as a nonsense path. Status code is useless here: the SPA answers 200
 * for everything.
 */
export function stellarChainEntityUrl(
  kind: 'transactions' | 'accounts' | 'contracts',
  id: string,
): string {
  return `${CURRENT_NETWORK.stellarChainUrl}/${kind}/${encodeURIComponent(id)}`;
}
