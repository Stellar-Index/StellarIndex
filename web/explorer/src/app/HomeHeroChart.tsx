'use client';

import Link from 'next/link';

import { MarketChart } from '@/components/charts/MarketChart';
import { useNativeUsdPrice } from '@/api/hooks';
import { FreshnessMarker } from '@/components/primitives';
import { cn } from '@/lib/cn';
import {
  isFrameStale,
  useLiveClock,
  usePriceFlash,
  useTipStream,
} from '@/lib/live/hooks';

/** A tip tick is "live" while fresher than this, matching the
 * LivePairPrice/LiveAssetPrice sibling widgets (30s of silence =
 * wedged stream or backgrounded tab). */
const TIP_LIVE_STALE_MS = 30_000;

/**
 * HomeHeroChart — a featured XLM/USD OHLC+volume chart on the landing
 * page so the home view leads with live price action, not just tables.
 * The headline price + 24h change come from /v1/price?asset=native (the
 * canonical XLM VWAP) — NOT /v1/assets, which excludes native XLM and
 * would resolve to USDC at ~$1.00. The candles come from /v1/ohlc.
 */
export function HomeHeroChart() {
  const { price, flags, change24hPct: change } = useNativeUsdPrice();
  const stale = flags.stale === true;
  // Make the "live USD price" label honest (RT-2): overlay the tip-price
  // stream on the build-time-baked initial and flash on each tick. A
  // frame older than TIP_LIVE_STALE_MS (stream wedged/quiet) must not
  // keep claiming "live" — same WB-04 rule the sibling widgets apply.
  const clock = useLiveClock();
  const tip = useTipStream('native');
  const tipFresh =
    tip != null && !isFrameStale(clock, tip.receivedAt, TIP_LIVE_STALE_MS);
  const tipStr = tipFresh ? tip.data.data.price : undefined;
  const tipActive = tipStr != null && Number.isFinite(Number(tipStr));
  const livePrice = tipActive
    ? Number(tipStr)
    : price != null
      ? Number(price)
      : null;
  const flash = usePriceFlash(tipActive ? tipStr : undefined);

  return (
    <section className="rounded-card border-line bg-surface shadow-card border p-5">
      <div className="mb-3 flex flex-wrap items-baseline justify-between gap-3">
        <div className="flex flex-wrap items-baseline gap-2.5">
          <Link
            href="/assets/XLM"
            className="text-h3 text-ink hover:text-brand-600 font-semibold"
          >
            XLM
          </Link>
          <span className="text-ink-muted text-sm">
            Stellar Lumens ·{' '}
            {tipActive
              ? 'live USD price'
              : stale
                ? 'USD price · stale'
                : 'USD price'}
          </span>
          {livePrice != null && (
            <span
              className={cn(
                'text-ink font-mono text-lg tabular-nums',
                flash === 'up' && 'flash-up',
                flash === 'down' && 'flash-down',
              )}
            >
              ${livePrice >= 1 ? livePrice.toFixed(4) : livePrice.toFixed(6)}
            </span>
          )}
          {/* stale is already in the caption; the rest (frozen above all)
              describe the pair, whichever transport carried the price. */}
          <FreshnessMarker flags={{ ...flags, stale: false }} />
          {change != null && (
            <span
              className={`font-mono text-sm tabular-nums ${
                change > 0
                  ? 'text-up'
                  : change < 0
                    ? 'text-down'
                    : 'text-ink-muted'
              }`}
            >
              {change > 0 ? '▲' : change < 0 ? '▼' : ''} {change > 0 ? '+' : ''}
              {change.toFixed(2)}% <span className="text-ink-faint">(24h)</span>
            </span>
          )}
        </div>
        <Link
          href="/assets/XLM"
          className="text-brand-600 text-xs hover:underline"
        >
          Full XLM detail →
        </Link>
      </div>
      <MarketChart
        base="native"
        quote="fiat:USD"
        baseLabel="XLM"
        quoteLabel="USD"
        height={300}
      />
    </section>
  );
}
