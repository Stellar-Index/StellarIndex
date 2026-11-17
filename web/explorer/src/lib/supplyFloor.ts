/**
 * True when a served supply is a provable floor (trustline or storage
 * balances only), so the supply and any value built on it are "at least".
 */
export function supplyIsFloor(a: {
  supply_basis?: string | null;
  value_lower_bound?: boolean;
  market_cap_lower_bound?: boolean;
}): boolean {
  return (
    a.value_lower_bound === true ||
    a.market_cap_lower_bound === true ||
    a.supply_basis === 'classic_trustline_sum' ||
    a.supply_basis === 'contract_storage_balances'
  );
}

export const SUPPLY_FLOOR_HINT =
  'At least: claimable, liquidity-pool and contract-held balances are not counted.';

export const VOLUME_FLOOR_HINT =
  'At least: trades without a trade-time USD value are not counted.';

/** True when the supply is derived from replayed mint/burn flows. */
export function supplyIsDerived(a: { supply_basis?: string | null }): boolean {
  return a.supply_basis === 'classic_lake_flows';
}

export const CLASSIC_FLOWS_CAVEAT =
  'Σ mint − burn − clawback over this classic asset’s Stellar Asset Contract events. A derived reading, not an issuer-certified supply: it is only as complete as the replayed flow history, and a replayed mint with no matching burn inflates it.';

/** Prefix for a figure that is a floor ('≥'), a derived upper reading ('≤'), or exact. */
export function supplyMark(floor: boolean, derived: boolean): string {
  return floor ? '≥ ' : derived ? '≤ ' : '';
}

export function supplyHint(
  floor: boolean,
  derived: boolean,
): string | undefined {
  return floor ? SUPPLY_FLOOR_HINT : derived ? CLASSIC_FLOWS_CAVEAT : undefined;
}
