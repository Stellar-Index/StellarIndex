import { Panel } from '@/components/reveal';

// CURATED_FAQ — generic answers parameterised by the asset's own code so the
// same five-question set renders sensibly for every asset. Extracted from
// page.tsx (2026-07-20) so the section is independently reorderable/testable;
// it's pure (no page data — just the asset code + whether it has an issuer).
//
// Exported because page.tsx also feeds it into the FAQPage JSON-LD schema —
// the visible panel and the structured data share ONE source of truth.
export function assetFaqFor(symbol: string, hasIssuer: boolean): { q: string; a: string }[] {
  const issuerNote = hasIssuer
    ? `As a classic credit asset, ${symbol} has a designated issuer account holding the canonical issuance authority — see the Issuer panel above for SEP-1 metadata, auth flags, and the home domain that pinned the issuer's identity.`
    : `As a Soroban-native or smart-contract token, ${symbol} doesn't have a classic Stellar issuer account. Its issuance is governed by the contract's own logic; on-chain mint/burn events drive its supply.`;
  return [
    {
      q: `What is ${symbol}?`,
      a: `${symbol} is one of the assets we index on the Stellar network. Stellar Index pulls live trades for it from the Soroban DEX corpus (Soroswap, Phoenix, Aquarius, Comet) plus the classic SDEX order book, plus CEX feeds (Binance, Coinbase, Kraken, Bitstamp) where the symbol exists. The price you see is a 24h-trailing VWAP across every active venue.`,
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
      q: `${symbol} issuer details`,
      a: issuerNote,
    },
    {
      q: `How fresh is this data?`,
      a: `On-chain trades land in the indexer within ~6 seconds of the ledger close (the Stellar consensus cadence). CEX feeds stream live via WebSocket; the 24h VWAP recomputes continuously. The chart's last-trade timestamp shows the most recent observation we ingested for this asset.`,
    },
  ];
}

export function AssetFAQ({ symbol, hasIssuer }: { symbol: string; hasIssuer: boolean }) {
  const items = assetFaqFor(symbol, hasIssuer);
  return (
    <Panel
      title="FAQ"
      hint="Common questions about this asset"
      bodyClassName="space-y-2 text-sm"
    >
      {items.map((it, i) => (
        <AssetFAQItem key={i} q={it.q} a={it.a} />
      ))}
    </Panel>
  );
}

function AssetFAQItem({ q, a }: { q: string; a: string }) {
  return (
    <details className="group rounded-lg border border-line">
      <summary className="flex cursor-pointer items-center justify-between px-3 py-2 font-medium text-ink hover:bg-surface-muted">
        <span>{q}</span>
        <span aria-hidden className="text-xs text-ink-faint group-open:rotate-45 transition-transform">+</span>
      </summary>
      <p className="border-t border-line px-3 py-2 text-sm leading-relaxed text-ink-body">
        {a}
      </p>
    </details>
  );
}
