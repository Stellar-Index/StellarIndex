import type { components, paths } from '@/api/types';

type S = components['schemas'];
type BadDebtData =
  paths['/lending/pools/{pool}/bad-debt']['get']['responses'][200]['content']['application/json']['data'];

type EnumReason =
  | S['LendingRevenueExclusion']['reason']
  | NonNullable<S['LendingRevenueUSD']['usd_reason']>
  | S['LendingRevenueUSDExclusion']['reason']
  | S['LendingRevenueAmounts']['realised']['fee_withdrawn_reason']
  | NonNullable<S['LendingBadDebtPart']['d_tokens_reason']>
  | NonNullable<S['LendingBadDebtReserve']['reconcile_reason']>
  | NonNullable<BadDebtData['backstop_reason']>;

/** Valuation reasons the spec names in prose on a bad-debt part's `usd_reason` / `underlying_reason`. */
export const BAD_DEBT_VALUATION_REASONS = [
  'rate_at_fill_unrecorded',
  'rate_unread',
  'no_price_within_bound',
  'price_withheld',
  'price_thin_market',
  'price_thin_at_fill',
  'price_unread',
  'current_rate_unavailable',
] as const;

type Reason = EnumReason | (typeof BAD_DEBT_VALUATION_REASONS)[number];

// Typed over the generated enums: a value the spec gains fails typecheck here.
export const REVENUE_REASON_TEXT: Record<Reason, string> = {
  interest_not_derived: 'interest is not derived yet for part of this range',
  derive_stale: 'revenue history is being recomputed',
  redo_pending: 'some hours are waiting to be recomputed',
  accrual_excluded: 'some rate changes could not be accrued',
  unexplained_supply_loss: 'suppliers lost value that no event explains',
  amounts_excluded: 'nothing could be summed',
  no_price_within_bound: 'no price close enough in time',
  price_missing: 'no price',
  unpriced: 'no USD market',
  decimals_unknown: 'token decimals unknown',
  price_scale_mismatch: 'price scale does not match the token',
  withheld: 'price withheld (flagged issuer or thin market)',
  price_read_failed: 'price could not be read',
  not_derivable: 'not yet derivable',
  not_applicable: 'not applicable',
  bid_not_recorded: 'not recorded on chain events for this pool version',
  amount_not_recorded: 'amount not recorded on chain events',
  asset_not_recorded: 'asset not recorded on chain events',
  backstop_unresolved: 'the pool’s backstop could not be identified',
  backstop_role_unverified: "BLNT's backstop address is not verified",
  lake_unavailable: 'raw ledger data is unavailable',
  position_unread: 'the backstop’s position could not be read',
  leg_kind_unknown: 'auction leg type not verified for this protocol',
  part_not_derivable: 'one of the parts is not derivable',
  as_of_mismatch: 'our event tables are behind the chain',
  rate_at_fill_unrecorded: 'exchange rate at the fill not recorded',
  rate_unread: 'exchange rate could not be read',
  price_withheld: 'price withheld',
  price_thin_market: 'market too thin to price',
  price_thin_at_fill: 'too little traded at the fill to price',
  price_unread: 'price could not be read',
  current_rate_unavailable: 'current exchange rate unavailable',
};

/** Plain words for a revenue or bad-debt reason; the raw code spaced out when unmapped. */
export function revenueReasonText(reason: string | null | undefined): string {
  if (!reason) return 'no value';
  return REVENUE_REASON_TEXT[reason as Reason] ?? reason.replace(/_/g, ' ');
}
