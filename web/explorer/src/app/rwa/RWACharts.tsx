import {
  DivergingColumns,
  HBarList,
  type HBarItem,
} from '@/components/charts/Bars';
import { formatDecimalAmount } from '@/lib/format';

export type ShareRow = {
  key: string;
  label: string;
  /** Served market-cap decimal string; null when no member is valued. */
  usd: string | null;
  /** Members without a valuation: any > 0 makes the figure a floor. */
  unvalued: number;
};

/**
 * Market cap per group as ranked bars. A group with unvalued members is
 * a floor, so it carries the "≥" in its label and the hatched tail;
 * groups with no valuation are counted in a note, never drawn as zero.
 */
export function ShareBars({
  rows,
  ariaLabel,
}: {
  rows: ShareRow[];
  ariaLabel: string;
}) {
  const valued = rows.filter((r) => r.usd != null && Number(r.usd) > 0);
  const omitted = rows.length - valued.length;
  const items: HBarItem[] = valued
    .map((r) => ({
      id: r.key,
      label: r.label,
      value: Number(r.usd),
      display: `${r.unvalued > 0 ? '≥ ' : ''}$${formatDecimalAmount(r.usd, 0)}`,
      annotation: r.unvalued > 0 ? `${r.unvalued} unvalued` : undefined,
      hatchTail: r.unvalued > 0,
      title:
        r.unvalued > 0
          ? 'A floor: members without a valuation are not counted.'
          : undefined,
    }))
    .sort((a, b) => b.value - a.value);
  if (items.length === 0) return null;
  return (
    <>
      <HBarList items={items} ariaLabel={ariaLabel} />
      {omitted > 0 && (
        <p className="text-ink-muted mt-1 text-xs">
          {omitted} group{omitted === 1 ? '' : 's'} with no valuation not shown
        </p>
      )}
    </>
  );
}

type PremiumAsset = {
  asset_id: string;
  code: string;
  premium: { status: string; pct?: string | null };
};

/** Published premiums only; a withheld premium has no bar, not a zero bar. */
export function PremiumBars({ assets }: { assets: PremiumAsset[] }) {
  const buckets = assets
    .filter((a) => a.premium.status === 'published' && a.premium.pct != null)
    .map((a) => ({
      a,
      raw: a.premium.pct as string,
      pct: Number(a.premium.pct),
    }))
    .filter((x) => Number.isFinite(x.pct))
    .sort((x, y) => y.pct - x.pct)
    .map(({ a, raw, pct }) => ({
      id: a.asset_id,
      label: a.code,
      pos: pct > 0 ? pct : 0,
      neg: pct < 0 ? -pct : 0,
      display: `${raw.replace(/^-/, '')}%`,
    }));
  // One bar compares nothing; the table row already carries the figure.
  if (buckets.length < 2) return null;
  return (
    <div className="border-line-subtle border-b px-4 pb-4">
      <h3 className="text-ink-muted mb-2 text-[11px] tracking-wider uppercase">
        Premium or discount now
      </h3>
      <DivergingColumns
        buckets={buckets}
        posLabel="Trades above instrument value"
        negLabel="Trades below instrument value"
        ariaLabel="Premium or discount to instrument value, per asset"
      />
    </div>
  );
}
