/**
 * True when a served supply is a provable floor (trustline or storage
 * balances only), so the supply and any value built on it are "at least".
 */
export function supplyIsFloor(a: {
  supply_basis?: string | null;
  value_lower_bound?: boolean;
}): boolean {
  return (
    a.value_lower_bound === true ||
    a.supply_basis === 'classic_trustline_sum' ||
    a.supply_basis === 'contract_storage_balances'
  );
}

export const SUPPLY_FLOOR_HINT =
  'At least: claimable, liquidity-pool and contract-held balances are not counted.';
