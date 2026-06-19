'use client';

import Link from 'next/link';

import { MarketChart } from '@/components/charts/MarketChart';
import { useNativeUsdPrice } from '@/api/hooks';

/**
 * HomeHeroChart — a featured XLM/USD OHLC+volume chart on the landing
 * page so the home view leads with live price action, not just tables.
 * The headline price + 24h change come from /v1/price?asset=native (the
 * canonical XLM VWAP) — NOT /v1/assets, which excludes native XLM and
 * would resolve to USDC at ~$1.00. The candles come from /v1/ohlc.
 */
export function HomeHeroChart() {
  const { price, change24hPct: change } = useNativeUsdPrice();

  return (
    <section className="rounded-card border border-line bg-surface p-5 shadow-card">
      <div className="mb-3 flex flex-wrap items-baseline justify-between gap-3">
        <div className="flex flex-wrap items-baseline gap-2.5">
          <Link href="/assets/XLM" className="text-h3 font-semibold text-ink hover:text-brand-600">
            XLM
          </Link>
          <span className="text-sm text-ink-muted">Stellar Lumens · live USD price</span>
          {price != null && (
            <span className="font-mono text-lg tabular-nums text-ink">
              ${price >= 1 ? price.toFixed(4) : price.toFixed(6)}
            </span>
          )}
          {change != null && (
            <span
              className={`font-mono text-sm tabular-nums ${
                change > 0 ? 'text-up' : change < 0 ? 'text-down' : 'text-ink-muted'
              }`}
            >
              {change > 0 ? '▲' : change < 0 ? '▼' : ''} {change > 0 ? '+' : ''}
              {change.toFixed(2)}% <span className="text-ink-faint">(24h)</span>
            </span>
          )}
        </div>
        <Link href="/assets/XLM" className="text-xs text-brand-600 hover:underline">
          Full XLM detail →
        </Link>
      </div>
      <MarketChart base="native" quote="fiat:USD" baseLabel="XLM" quoteLabel="USD" height={300} />
    </section>
  );
}
