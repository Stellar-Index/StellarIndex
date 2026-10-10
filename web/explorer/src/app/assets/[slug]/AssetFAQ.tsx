// Generic answers parameterised by the asset's own code. Pure; page.tsx feeds
// them into the FAQPage JSON-LD schema only (no visible panel).
export function assetFaqFor(
  symbol: string,
  kind: 'native' | 'classic' | 'contract',
): { q: string; a: string }[] {
  const issuerNote =
    kind === 'native'
      ? `XLM has no issuer. It is the network's own asset, created at genesis.`
      : kind === 'classic'
        ? `As a classic credit asset, ${symbol} has a designated issuer account holding the canonical issuance authority — see the Issuer panel above for SEP-1 metadata, auth flags, and the home domain that pinned the issuer's identity.`
        : `As a Soroban-native or smart-contract token, ${symbol} doesn't have a classic Stellar issuer account. Its issuance is governed by the contract's own logic; on-chain mint/burn events drive its supply.`;
  return [
    {
      q: `What is ${symbol}?`,
      a:
        kind === 'native'
          ? `Stellar Lumens (XLM) is the native asset of the Stellar network. It pays transaction fees and account reserves.`
          : kind === 'classic'
            ? `${symbol} is a classic Stellar asset, identified by its code and issuer account.`
            : `${symbol} is a Soroban token, identified by its contract address.`,
    },
    {
      q: `Where does the price come from?`,
      a: `We compute a volume-weighted average across every connected exchange that's actively trading ${symbol} in the trailing 24 hours. Source-class exchanges (CEX + on-chain DEX) contribute by default; aggregators and oracles are reported alongside but excluded from the VWAP itself to avoid double-counting upstream markets.`,
    },
    {
      q: `What is circulating supply for a Stellar asset?`,
      a: `For classic credit assets we use the issuer's current balance held by non-issuer accounts (the on-chain definition of "in circulation"); for Soroban tokens we track mint/burn events on the contract. SEP-1 fixed_number / max_number declarations from the issuer's stellar.toml override the on-chain count when the issuer pledges a hard cap.`,
    },
    {
      q: `Who issues ${symbol}?`,
      a: issuerNote,
    },
    {
      q: `How fresh is this data?`,
      a: `On-chain trades land in the indexer within ~6 seconds of the ledger close (the Stellar consensus cadence). CEX feeds stream live via WebSocket; the 24h VWAP recomputes continuously. The chart's last-trade timestamp shows the most recent observation we ingested for this asset.`,
    },
  ];
}
