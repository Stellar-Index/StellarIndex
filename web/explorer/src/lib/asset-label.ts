// Canonical asset-id display/slug helpers, server-safe: components/AssetLink.tsx
// is 'use client', and server components cannot call its exports (RSC turns
// them into throwing client references). AssetLink re-exports these.

import { CURRENT_NETWORK_ID, type NetworkId } from '@/lib/networks';

// The native-XLM Stellar Asset Contract is a pure function of the network
// passphrase, so each network has its own id.
const NATIVE_XLM_SAC: Record<NetworkId, string> = {
  mainnet: 'CAS3J7GYLGXMF6TDJBBYYSE3HQ6BBSMLNUQ34T6TZMYMW2EVH34XOWMA',
  testnet: 'CDLZFC3SYJYDZT7K67VZ75HPJVIEUVNIXF47ZG2FB2RMQQVU2HHGCYSC',
  futurenet: 'CB64D3G7SM2RTH6JSGG34DDTFTQ5CFDKVDZJZSODMCX4NJ2HV2KN7OHT',
};

/** True when `id` is the native-XLM SAC of the network this build serves. */
export function isNativeXlmSac(
  id: string | null | undefined,
  network: NetworkId = CURRENT_NETWORK_ID,
): boolean {
  return !!id && id === NATIVE_XLM_SAC[network];
}

/**
 * normalizeColonForm rewrites a classic-asset id served in the colon
 * form (`CODE:G...`) to the canonical dash form (`CODE-G...`). Only
 * a short all-alnum code followed by `:G<55 base32>` qualifies, so
 * `pool:<hex>` liquidity-pool ids and raw Soroban contract ids (which
 * never contain a colon) are left untouched.
 */
export function normalizeColonForm(canonical: string): string {
  return /^[A-Za-z0-9]{1,12}:G[A-Z2-7]{55}$/.test(canonical)
    ? canonical.replace(':', '-')
    : canonical;
}

/**
 * RAW_ORACLE_PREFIX — the `raw:<symbol>` namespace an oracle-published
 * symbol is recorded under when it maps to no canonical asset (oracle
 * capture-totality; canonical.AssetOracleRaw). Reference-only: never
 * compared, aggregated, or linked to an asset page. The explorer renders
 * the on-wire symbol verbatim (monospace, unlinked) and keeps such rows
 * out of every mapped list.
 */
export const RAW_ORACLE_PREFIX = 'raw:';

export function isRawOracleAsset(
  canonical: string | undefined | null,
): boolean {
  return !!canonical && canonical.startsWith(RAW_ORACLE_PREFIX);
}

/** rawOracleSymbol — the on-wire symbol behind a `raw:<symbol>` id. */
export function rawOracleSymbol(canonical: string): string {
  return canonical.slice(RAW_ORACLE_PREFIX.length);
}

/**
 * shortAssetText — compact single-line label for a canonical asset_id,
 * for dense table cells where AssetLabel's two-line form is too tall.
 * THE canonical (page-local forks are not allowed): they
 * rendered numeric trustline ids as raw digits and a
 * C… Soroban id as all 56 chars.
 */
export function shortAssetText(canonical: string | undefined | null): string {
  if (!canonical) return '—';
  if (canonical === 'native' || /^\d+$/.test(canonical)) return 'XLM';
  if (canonical.startsWith('fiat:')) return canonical.slice(5);
  if (canonical.startsWith('crypto:')) return canonical.slice(7);
  // Unmapped oracle symbol — verbatim, never truncated (it IS the evidence).
  if (isRawOracleAsset(canonical)) return rawOracleSymbol(canonical);
  if (/^C[A-Za-z0-9]{55}$/.test(canonical))
    return `${canonical.slice(0, 4)}…${canonical.slice(-4)}`;
  canonical = normalizeColonForm(canonical);
  const i = canonical.indexOf('-');
  if (i === -1)
    return canonical.length > 12
      ? `${canonical.slice(0, 4)}…${canonical.slice(-4)}`
      : canonical;
  return canonical.slice(0, i);
}
