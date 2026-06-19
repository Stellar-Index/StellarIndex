'use client';

import { MarketChart } from '@/components/charts/MarketChart';

/**
 * PairChart — the (base, quote) price chart for a market pair detail
 * page. Thin wrapper over the shared MarketChart (real OHLC + volume
 * from /v1/ohlc); quote is fixed to the URL pair.
 */
export function PairChart({
  base,
  quote,
  baseLabel,
  quoteLabel,
}: {
  base: string;
  quote: string;
  baseLabel: string;
  quoteLabel: string;
}) {
  return (
    <MarketChart base={base} quote={quote} baseLabel={baseLabel} quoteLabel={quoteLabel} />
  );
}
