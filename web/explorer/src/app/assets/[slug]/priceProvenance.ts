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
