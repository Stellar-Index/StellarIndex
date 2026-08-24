import { API_BASE_URL } from '@/api/client';
import { isCIStub } from '@/lib/buildFetch';

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

// CI builds use a stub hostname that doesn't resolve; bypass the
// network fetch in that case so static export doesn't time out.

const BUILD_FETCH_TIMEOUT_MS = 8_000;

/**
 * fetchVerifiedCurrencies is the shared `/v1/assets/verified`
 * fetcher consumed by both this strip and the AssetsTable. Single
 * server-side fetch per page render — the page calls this once,
 * passes the result to both components as a prop.
 */
export async function fetchVerifiedCurrencies(): Promise<VerifiedItem[]> {
  if (isCIStub) return [];
  try {
    const res = await fetch(`${API_BASE_URL}/v1/assets/verified`, {
      signal: AbortSignal.timeout(BUILD_FETCH_TIMEOUT_MS),
    });
    if (!res.ok) return [];
    const env = (await res.json()) as { data?: VerifiedItem[] };
    return env.data ?? [];
  } catch {
    return [];
  }
}
