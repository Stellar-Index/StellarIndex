import type { components } from '@/api/types';
import type { PriceProvenance } from './LiveAssetPrice';

export type PriceBasis = components['schemas']['Asset']['price_basis'];

/**
 * Map the wire's `price_basis` to the caption vocabulary. `direct` is
 * what an absent basis (a direct market price) means to the caller.
 * An unrecognised basis maps to null: a wrong caption states a
 * provenance the price does not have, which is worse than none. The
 * `never` arm makes a new spec basis a typecheck failure here.
 */
export function provenanceFromBasis(
  basis: PriceBasis,
  direct: 'vwap1m' | 'listing',
): PriceProvenance {
  switch (basis) {
    case undefined:
      return direct;
    case 'global_market':
      return 'global_market';
    case 'declared_peg':
      return 'declared_peg';
    case 'transitive':
      return 'transitive';
    default: {
      const unknown: never = basis;
      void unknown;
      return null;
    }
  }
}

/**
 * A price /v1/price and /v1/price/batch cannot answer for: it is re-read
 * from /v1/assets, and a withheld or absent market verdict leaves it standing.
 */
export function isDerivedProvenance(p: PriceProvenance): boolean {
  return p === 'global_market' || p === 'declared_peg' || p === 'transitive';
}

/**
 * What a non-market or non-live price IS, in a few words; null for a live
 * market price. Shared by the converter so it names what the headline names.
 */
export function provenanceQualifier(p: PriceProvenance): string | null {
  switch (p) {
    case 'declared_peg':
      return 'declared 1:1 fiat peg × fx rate, not a market price';
    case 'transitive':
      return 'derived two-hop DEX route';
    case 'global_market':
      return 'global aggregator price, not a Stellar market';
    case 'listing':
      return 'listing snapshot, not a live aggregated price';
    default:
      return null;
  }
}

export const BAKED_AT_DEPLOY = 'as baked at deploy';

/** The caption for a figure frozen into the static export, with its build time when known. */
export function bakedAtDeploy(
  builtAt: string | undefined = process.env.NEXT_PUBLIC_BUILD_TIME,
): string {
  const t = builtAt ? new Date(builtAt) : null;
  if (!t || Number.isNaN(t.getTime())) return BAKED_AT_DEPLOY;
  return `${BAKED_AT_DEPLOY} (built ${t.toISOString().slice(0, 16).replace('T', ' ')} UTC)`;
}

/** Caption vocabulary for the headline price on the static asset page. */
export function headlinePriceProvenance(
  price:
    | { price?: string | null; flags?: { triangulated?: boolean } }
    | null
    | undefined,
  coin: { price_usd?: string | null; price_basis?: PriceBasis },
): PriceProvenance {
  if (price?.price)
    return price.flags?.triangulated ? 'triangulated' : 'vwap1m';
  if (!coin.price_usd) return null;
  return provenanceFromBasis(coin.price_basis, 'listing');
}
