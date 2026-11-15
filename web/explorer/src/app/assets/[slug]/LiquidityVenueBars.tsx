import { HBarList, type HBarItem } from '@/components/charts/Bars';
import { formatReadable, sumDecimalStrings } from '@/lib/format';
import { PANEL_LABEL_CLASS } from '@/components/ui/Page';

type PoolVolume = {
  source: string;
  volume_24h_usd?: string | null;
  volume_lower_bound?: boolean;
};

/**
 * Per-venue 24h USD volume, ranked. Sums are exact decimal-string sums;
 * a venue with an unvalued pool, or one whose volume excludes unvalued
 * trades, keeps the lower-bound hatch. Pools reports volume, not reserves, so this ranks
 * activity, not TVL. When the pool list was cut off (truncated), every
 * venue's sum misses the pools past the cut and is a lower bound.
 */
export function buildVenueRows(
  pools: PoolVolume[],
  truncated = false,
): HBarItem[] {
  const by = new Map<string, PoolVolume[]>();
  for (const p of pools) by.set(p.source, [...(by.get(p.source) ?? []), p]);
  const rows: HBarItem[] = [];
  for (const [source, ps] of by) {
    const sum = sumDecimalStrings(ps.map((p) => p.volume_24h_usd));
    if (sum == null) continue;
    const lower =
      truncated ||
      ps.some((p) => p.volume_lower_bound === true || p.volume_24h_usd == null);
    rows.push({
      label: source,
      value: Number(sum),
      display: `${lower ? '≥ ' : ''}${formatReadable(sum, true)}`,
      annotation: `${ps.length}${truncated ? '+' : ''} pool${ps.length === 1 && !truncated ? '' : 's'}`,
      hatchTail: lower,
      title: !lower
        ? `$${sum}`
        : truncated
          ? `≥ $${sum}. Lower bound: only the most active pools are listed, so this venue's smaller pools are not in the sum.`
          : `≥ $${sum}. Lower bound: unvalued pools and trades without a trade-time USD value are excluded.`,
    });
  }
  return rows.sort((a, b) => b.value - a.value);
}

export function LiquidityVenueBars({
  pools,
  truncated = false,
}: {
  pools: PoolVolume[];
  truncated?: boolean;
}) {
  const items = buildVenueRows(pools, truncated);
  // One venue compares nothing; the table below already names it.
  if (items.length < 2) return null;
  return (
    <div className="px-4 pb-3">
      <h3 className={`${PANEL_LABEL_CLASS} mb-2`}>24h volume by venue</h3>
      <HBarList items={items} ariaLabel="24h USD volume by venue" />
    </div>
  );
}
