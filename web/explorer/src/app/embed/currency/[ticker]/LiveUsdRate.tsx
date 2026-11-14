'use client';

import { formatPairPrice } from '@/lib/format';

import { useLivePrice } from '../../LivePrice';

/**
 * LiveUsdRate — the "1 USD = X" footer, derived from the same price the
 * headline shows so the two cannot disagree after the first live poll.
 */
export function LiveUsdRate({
  ticker,
  initialInverseUsd,
}: {
  ticker: string;
  initialInverseUsd: string;
}) {
  const live = useLivePrice(`fiat:${ticker}`);
  if (live === null) return null;
  const rate = 1 / Number(live ?? initialInverseUsd);
  if (!Number.isFinite(rate) || rate <= 0) return null;
  return (
    <span className="font-mono tabular-nums">
      1 USD = {formatRate(rate)} {ticker}
    </span>
  );
}

function formatRate(n: number): string {
  return n >= 1000
    ? n.toLocaleString('en-US', { maximumFractionDigits: 2 })
    : formatPairPrice(n);
}
