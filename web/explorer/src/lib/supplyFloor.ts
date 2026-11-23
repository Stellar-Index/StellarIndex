import { compareDecimalStrings } from '@/lib/format';

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

type SupplyFigures = Parameters<typeof supplyIsFloor>[0] & {
  circulating_supply_lower_bound?: boolean;
  total_supply?: string | null;
  circulating_supply?: string | null;
};

/**
 * True when the circulating supply (and the market cap built on it) is a
 * floor. Judged only from the payload that served the figure: another
 * endpoint's flag describes a different reading.
 */
export function circulatingSupplyIsFloor(a: SupplyFigures): boolean {
  return supplyIsFloor(a) || a.circulating_supply_lower_bound === true;
}

/**
 * True when the served total is a floor: its own basis says so, or it is the
 * same reading as a circulating figure the payload calls a floor (a lake-net
 * reading sets total == circulating).
 */
export function totalSupplyIsFloor(a: SupplyFigures): boolean {
  return (
    supplyIsFloor(a) ||
    (a.circulating_supply_lower_bound === true &&
      a.total_supply != null &&
      a.circulating_supply != null &&
      compareDecimalStrings(a.total_supply, a.circulating_supply) === 0)
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
