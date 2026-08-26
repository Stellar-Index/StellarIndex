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
}

// Ordered mainnet → testnet → futurenet (production first, protocol-upgrade
// pipeline order thereafter).
export const NETWORKS: NetworkInfo[] = [
  {
    id: 'mainnet',
    label: 'Mainnet',
    tag: 'mainnet',
    explorerUrl: 'https://stellarindex.io',
    apiBaseUrl: 'https://api.stellarindex.io',
    live: true,
  },
  {
    id: 'testnet',
    label: 'Testnet',
    tag: 'testnet',
    explorerUrl: 'https://testnet.stellarindex.io',
    apiBaseUrl: 'https://api.testnet.stellarindex.io',
    live: true,
  },
  {
    id: 'futurenet',
    label: 'Futurenet',
    tag: 'futurenet',
    explorerUrl: 'https://futurenet.stellarindex.io',
    apiBaseUrl: 'https://api.futurenet.stellarindex.io',
    // Phase 2 — VM not provisioned yet. Flip to true when it goes live.
    live: false,
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
