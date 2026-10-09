'use client';

import { DonutChart } from '@/components/charts/DonutChart';
import { topSlices } from '@/components/charts/topSlices';

/**
 * Positions per protocol. Counts, not amounts: DeFi position amounts are
 * raw quantities in mixed units with no valuation, so they cannot be summed.
 */
export function AccountDefiProtocolMix({
  counts,
}: {
  counts: { protocol: string; label: string; count: number }[];
}) {
  const slices = topSlices(
    counts
      .filter((c) => c.count > 0)
      .map((c) => ({
        id: c.protocol,
        label: c.label,
        value: c.count,
        decimal: String(c.count),
      })),
    5,
  );
  if (slices.length < 2) return null;
  const total = counts.reduce((n, c) => n + c.count, 0);
  return (
    <div>
      <div className="text-ink-muted mb-2 text-[11px] tracking-wider uppercase">
        Positions by protocol — count, not value
      </div>
      <DonutChart
        data={slices}
        size={130}
        thickness={18}
        centerLabel={String(total)}
        centerSub="positions"
      />
    </div>
  );
}
