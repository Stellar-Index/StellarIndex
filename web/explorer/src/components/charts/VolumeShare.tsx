'use client';

import { HBarList } from '@/components/charts/Bars';
import {
  CATEGORICAL_PALETTE,
  DonutChart,
} from '@/components/charts/DonutChart';
import {
  compareDecimalStrings,
  formatCompact,
  formatCompactUnits,
  sumDecimalStrings,
} from '@/lib/format';

export type VolumeShareRow = {
  id: string;
  label: string;
  /** 24h USD volume, decimal string (ADR-0003). */
  volume: string | null | undefined;
};

/**
 * topWithOther — rows with a positive volume, largest first, cut to `n`
 * with the remainder summed exactly into one `other` bucket so the donut
 * always totals the full listed set.
 */
export function topWithOther(rows: readonly VolumeShareRow[], n: number) {
  const priced = rows
    .filter(
      (r): r is VolumeShareRow & { volume: string } =>
        r.volume != null && compareDecimalStrings(r.volume, '0') === 1,
    )
    .sort((a, b) => -(compareDecimalStrings(a.volume, b.volume) ?? 0));
  const top = priced.slice(0, n);
  const rest = priced.slice(n);
  return {
    top,
    otherCount: rest.length,
    other: sumDecimalStrings(rest.map((r) => r.volume)),
    total: sumDecimalStrings(priced.map((r) => r.volume)),
    pricedCount: priced.length,
  };
}

/**
 * VolumeShare — ranked volume bars plus a share donut for a capped list.
 * `noun` names the rows ("pairs"); `scopeNote` says what the list itself
 * excludes, so a partial list never reads as the whole market.
 */
export function VolumeShare({
  rows,
  topN = 10,
  noun,
  scopeNote,
}: {
  rows: readonly VolumeShareRow[];
  topN?: number;
  noun: string;
  scopeNote?: string;
}) {
  const { top, other, otherCount, total, pricedCount } = topWithOther(
    rows,
    topN,
  );
  if (top.length === 0 || total == null) return null;

  const slices = top.map((r, i) => ({
    id: r.id,
    label: r.label,
    value: Number(r.volume),
    decimal: r.volume,
    color: CATEGORICAL_PALETTE[i % (CATEGORICAL_PALETTE.length - 1)],
  }));
  if (other != null) {
    slices.push({
      id: '__other',
      label: `Other (${otherCount})`,
      value: Number(other),
      decimal: other,
      color: CATEGORICAL_PALETTE[CATEGORICAL_PALETTE.length - 1],
    });
  }

  return (
    <div className="space-y-2" data-testid="volume-share">
      <div className="grid items-center gap-6 md:grid-cols-[1fr_auto]">
        <HBarList
          ariaLabel={`Top ${top.length} ${noun} by 24h USD volume`}
          items={slices.slice(0, top.length).map((s) => ({
            id: s.id,
            label: s.label,
            value: s.value,
            display: `$${formatCompactUnits(s.decimal)}`,
            color: s.color,
          }))}
        />
        <DonutChart
          data={slices}
          centerLabel={`$${formatCompactUnits(total)}`}
          centerSub={`${pricedCount} ${noun}`}
          formatValue={(n) => `$${formatCompact(n)}`}
        />
      </div>
      <p className="text-ink-muted text-xs">
        Top {top.length} of {pricedCount} {noun} with priced 24h volume
        {other != null ? `; the other ${otherCount} are one slice` : ''}.
        {scopeNote ? ` ${scopeNote}` : ''}
      </p>
    </div>
  );
}
