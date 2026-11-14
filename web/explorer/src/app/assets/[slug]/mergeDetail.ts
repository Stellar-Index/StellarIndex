import type { components } from '@/api/types';

type Asset = components['schemas']['Asset'];

// The detail withholds a dollar figure by omitting its key, so a listing
// row's value for any of these must not survive under the detail.
const DETAIL_OWNED: (keyof Asset)[] = [
  'price_usd',
  'price_basis',
  'market_cap_usd',
  'fdv_usd',
  'change_1h_pct',
  'change_24h_pct',
  'change_7d_pct',
  'price_history_24h',
  'price_history_7d',
  'ath',
];

export function mergeDetailOverListing<
  L extends Partial<Record<keyof Asset, unknown>>,
  D extends object,
>(listing: L, detail: D): L & D {
  const base: Partial<Record<keyof Asset, unknown>> = { ...listing };
  for (const k of DETAIL_OWNED) delete base[k];
  return { ...(base as L), ...detail };
}
