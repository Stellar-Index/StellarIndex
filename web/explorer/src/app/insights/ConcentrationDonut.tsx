'use client';

import { DonutChart } from '@/components/charts/DonutChart';
import { formatCompact, ratioPct } from '@/lib/format';

export interface ConcentrationRow {
  id: string;
  label: string;
  /** Exact integer count (not money). */
  count: number;
  href?: string;
}

/**
 * ConcentrationDonut — how much of a network-wide count the top entities
 * hold. `total` is the served all-entity count, so the remainder slice is
 * everyone not listed. Fewer than two listed entities compare nothing.
 */
export function ConcentrationDonut({
  rows,
  total,
  noun,
  what,
  topN = 5,
}: {
  rows: readonly ConcentrationRow[];
  total: number;
  noun: string;
  what: string;
  topN?: number;
}) {
  const top = rows
    .filter((r) => Number.isFinite(r.count) && r.count > 0)
    .sort((a, b) => b.count - a.count)
    .slice(0, topN);
  if (top.length < 2) return null;
  const topSum = top.reduce((s, r) => s + r.count, 0);
  const whole = Math.max(total, topSum);
  const rest = whole - topSum;
  const slices = top.map((r) => ({
    id: r.id,
    label: r.label,
    value: r.count,
    decimal: String(r.count),
    href: r.href,
  }));
  if (rest > 0)
    slices.push({
      id: '__rest',
      label: `All other ${noun}`,
      value: rest,
      decimal: String(rest),
      href: undefined,
    });
  const pct = ratioPct(String(topSum), String(whole), 1);

  return (
    <div className="space-y-2" data-testid="concentration-donut">
      <DonutChart
        data={slices}
        centerLabel={pct == null ? undefined : `${pct}%`}
        centerSub={`top ${top.length}`}
        formatValue={formatCompact}
      />
      <p className="text-ink-muted text-xs">
        Top {top.length} {noun} hold {pct ?? '—'}% of all {what}.
      </p>
    </div>
  );
}
