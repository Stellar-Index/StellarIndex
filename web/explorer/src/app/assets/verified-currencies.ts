import { buildFetchData } from '@/lib/buildFetch';

/**
 * Mirror of `VerifiedItemListItem` on the wire.
 * See `internal/api/v1/assets_global.go`.
 */
export interface VerifiedItem {
  ticker: string;
  slug: string;
  name: string;
  class?: 'crypto' | 'stablecoin' | 'fiat';
  verified_issuer?: string;
  // market_cap_usd is populated for fiat rows by /v1/assets/verified
  // (R-018 assets-unification step 5). Decimal string with 2
  // fractional digits. Empty for crypto/stablecoin rows.
  market_cap_usd?: string;
}

/**
 * fetchVerifiedCurrencies is the shared `/v1/assets/verified`
 * fetcher consumed by both this strip and the AssetsTable. Single
 * server-side fetch per page render — the page calls this once,
 * passes the result to both components as a prop.
 *
 * Goes through buildFetchData's fail-hard contract (src/lib/buildFetch.ts):
 * a persistent transport failure THROWS and fails the build instead of
 * silently baking a catalogue with zero verified rows. A scam token
 * impersonating a verified ticker relies on exactly that kind of quiet
 * degradation — see the (code, issuer) rule in AGENTS.md.
 */
export async function fetchVerifiedCurrencies(): Promise<VerifiedItem[]> {
  const data = await buildFetchData<VerifiedItem[]>('/v1/assets/verified');
  return data ?? [];
}
