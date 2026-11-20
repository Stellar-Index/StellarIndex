'use client';

import type { RequestExample } from '@/api/client';
import { Panel } from '@/components/reveal';
import { formatPriceSmall, positiveDecimal } from '@/lib/format';

import { useResolvedAssetPrice, type PriceProvenance } from './LiveAssetPrice';

/**
 * The 24h price range bar. The series is baked at deploy, so it says so; the
 * marker sits at the headline's resolved price, and the panel is withdrawn
 * with it when the server withholds the price.
 */
export function PerformanceRange({
  assetId,
  priceUSD,
  priceProvenance,
  priceStale,
  points,
  source,
}: {
  assetId: string;
  priceUSD: string | null;
  priceProvenance: PriceProvenance;
  priceStale?: boolean;
  points: { t: string; p?: string | null }[];
  source: RequestExample;
}) {
  const resolved = useResolvedAssetPrice(
    assetId,
    priceUSD,
    priceProvenance,
    priceStale,
  );
  const live = resolved.withheld ? null : positiveDecimal(resolved.price);
  if (live == null) return null;
  // Bar geometry only; no displayed amount is derived from these floats.
  const vals = points
    .map((pt) => (pt.p != null ? Number(pt.p) : null))
    .filter((v): v is number => v != null && Number.isFinite(v));
  if (vals.length < 2) return null;
  const low = Math.min(...vals);
  const high = Math.max(...vals);
  const cur = Number(live);
  const pct =
    high > low
      ? Math.max(0, Math.min(100, ((cur - low) / (high - low)) * 100))
      : 50;
  return (
    <Panel
      headingLevel={2}
      title="Price range"
      hint="24h · USD · as baked at deploy"
      source={source}
    >
      <div className="text-ink-muted flex items-center justify-between text-[11px]">
        <span>Low</span>
        <span>High</span>
      </div>
      <div className="bg-surface-muted relative mt-1 h-1.5 rounded-full">
        <div
          className="bg-ink absolute top-1/2 h-3 w-0.5 -translate-y-1/2 rounded-full"
          style={{ left: `calc(${pct}% - 1px)` }}
          aria-hidden
        />
      </div>
      <div className="text-ink-body mt-1.5 flex items-baseline justify-between font-mono text-xs tabular-nums">
        <span>${formatPriceSmall(low)}</span>
        <span>${formatPriceSmall(high)}</span>
      </div>
    </Panel>
  );
}
