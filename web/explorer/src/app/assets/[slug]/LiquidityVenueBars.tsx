import { HBarList, type HBarItem } from '@/components/charts/Bars';
import { formatReadable, sumDecimalStrings } from '@/lib/format';

type PoolVolume = {
  source: string;
  volume_24h_usd?: string | null;
  volume_lower_bound?: boolean;
};

/**
 * Per-venue 24h USD volume, ranked. Sums are exact decimal-string sums;
 * a venue with an unvalued pool, or one whose volume excludes unvalued
 * trades, keeps the lower-bound hatch. Pools reports volume, not reserves, so this ranks
 * activity, not TVL.
 */
export function buildVenueRows(pools: PoolVolume[]): HBarItem[] {
  const by = new Map<string, PoolVolume[]>();
  for (const p of pools) by.set(p.source, [...(by.get(p.source) ?? []), p]);
  const rows: HBarItem[] = [];
  for (const [source, ps] of by) {
    const sum = sumDecimalStrings(ps.map((p) => p.volume_24h_usd));
    if (sum == null) continue;
    const lower = ps.some(
      (p) => p.volume_lower_bound === true || p.volume_24h_usd == null,
    );
    rows.push({
      label: source,
      value: Number(sum),
      display: `${lower ? '≥ ' : ''}${formatReadable(sum, true)}`,
      annotation: `${ps.length} pool${ps.length === 1 ? '' : 's'}`,
      hatchTail: lower,
      title: lower
        ? `≥ $${sum}. Lower bound: unvalued pools and trades without a trade-time USD value are excluded.`
        : `$${sum}`,
    });
  }
  return rows.sort((a, b) => b.value - a.value);
}

export function LiquidityVenueBars({ pools }: { pools: PoolVolume[] }) {
  const items = buildVenueRows(pools);
  // One venue compares nothing; the table below already names it.
  if (items.length < 2) return null;
  return (
    <div className="px-4 pb-3">
      <h3 className="text-ink-muted mb-2 text-[11px] font-semibold tracking-wider uppercase">
        24h volume by venue
      </h3>
      <HBarList items={items} ariaLabel="24h USD volume by venue" />
    </div>
  );
}
