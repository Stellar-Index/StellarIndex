import type { components } from '@/api/types';
import { Badge } from '@/components/ui';
import { formatCompact } from '@/lib/format';

export type SubstanceEvidence = components['schemas']['SubstanceEvidence'];

function hours(seconds: number): string {
  const h = seconds / 3600;
  return h === 1 ? '1 hour' : `${Number.isInteger(h) ? h : h.toFixed(1)} hours`;
}

/** thinMarketNote explains why a thin-market price is low confidence. */
export function thinMarketNote(s?: SubstanceEvidence | null): string {
  const base =
    'Low confidence: this price comes from a market too thin to pass our substance floor. ' +
    'It is shown for reference only; no market cap, price change or total uses it.';
  if (!s) return base;
  return (
    `${base} Over the last ${hours(s.window_seconds)} it traded ` +
    `$${formatCompact(s.volume_usd)} (floor $${formatCompact(s.floor.min_volume_usd)}) ` +
    `in ${s.buckets} active price buckets (floor ${s.floor.min_buckets}).`
  );
}

// ThinMarketBadge — marks a price served under `include_thin=true` from a
// market below the substance floor. The tooltip carries the measurement
// when the response included it (detail and /v1/price; listings do not).
export function ThinMarketBadge({
  substance,
  className,
}: {
  substance?: SubstanceEvidence | null;
  className?: string;
}) {
  const note = thinMarketNote(substance);
  return (
    <Badge tone="warn" className={className} title={note} aria-label={note}>
      ⚠
    </Badge>
  );
}
