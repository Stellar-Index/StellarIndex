// xlm-supply — the one place the explorer says what the ledger header's
// `total_coins` IS, so every surface that prints it captions it the same way.
//
// Audit 2026-08-28 ("XLM supply 2.11× route divergence"): on mainnet the
// header's total_coins is ~105B XLM because that on-chain field was never
// lowered by the October 2019 burn — it still counts the ~55B lumens SDF
// destroyed — while /v1/assets/native serves the market's 50.0B total
// supply (docs/methodology/xlm-circulating-supply.md). An unlabeled 105B
// next to a 50B reads as a bug, so total_coins is never shown bare.
//
// Only mainnet has the burn story; testnet's genesis is 100B with no burn
// and futurenet likewise, so there the caption just names the source.
import { type NetworkId, CURRENT_NETWORK_ID } from './networks';

/** Caption for a mainnet `total_coins` figure. */
export const TOTAL_COINS_CAPTION_MAINNET =
  'ledger header · includes the 2019 burn';
/** Caption for a `total_coins` figure on a network with no burn. */
export const TOTAL_COINS_CAPTION_TESTNET = 'ledger header';

/**
 * The caption to print under a ledger header's `total_coins`. Reads the
 * network at CALL time (not module load) so callers need no re-import to
 * follow a network switch in tests.
 */
export function totalCoinsCaption(
  networkId: NetworkId = CURRENT_NETWORK_ID,
): string {
  return networkId === 'mainnet'
    ? TOTAL_COINS_CAPTION_MAINNET
    : TOTAL_COINS_CAPTION_TESTNET;
}
