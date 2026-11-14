import type { components } from '@/api/types';

type Reason = NonNullable<
  components['schemas']['Asset']['price_withheld_reason']
>;

const WITHHELD: Record<Reason, string> = {
  substance: 'withheld · thin market',
  scam_issuer: 'withheld · flagged issuer',
  upstream_leg: 'withheld · USD leg withheld',
  unattributed: 'withheld',
};

// A null market cap reads as "unknown"; when the server withheld the price
// the chip says why instead.
export function withheldNote(
  reason: Reason | undefined | null,
): string | undefined {
  return reason ? WITHHELD[reason] : undefined;
}

export function supplyAsOfNote(
  asOf: string | undefined | null,
): string | undefined {
  if (!asOf) return undefined;
  const d = new Date(asOf);
  return Number.isNaN(d.getTime())
    ? undefined
    : `as of ${d.toISOString().slice(0, 10)}`;
}

// A null market cap with no withheld price still has a served reason.
export function marketCapNote(a: {
  price_withheld_reason?: Reason | null;
  market_cap_decimals_mismatch?: boolean;
  market_cap_low_liquidity?: boolean;
}): string | undefined {
  return (
    withheldNote(a.price_withheld_reason) ??
    (a.market_cap_decimals_mismatch
      ? 'refused · decimals disagree'
      : a.market_cap_low_liquidity
        ? 'withheld · negligible liquidity'
        : undefined)
  );
}
