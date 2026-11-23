import {
  DivergingColumns,
  HBarList,
  type HBarItem,
} from '@/components/charts/Bars';
import { formatReadable } from '@/lib/format';

export type ShareRow = {
  key: string;
  label: string;
  /** Served `reference_value_usd`; null when no member has a reference valuation. */
  usd: string | null;
  /** `assets_reference_unvalued`: any > 0 makes the figure a lower bound. */
  unvalued: number;
  /** `reference_value_lower_bound`: the API's own partial-total flag. */
  lowerBound?: boolean;
};

const assetsText = (n: number) => `${n} asset${n === 1 ? '' : 's'}`;

/**
 * Value of backing (by reference valuation) per group as ranked bars, never
 * market cap. A lower bound carries "≥" and a hatched tail; a group with no
 * reference valuation is counted in a note, never drawn as zero.
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
    .map((r) => {
      const floor = r.lowerBound === true || r.unvalued > 0;
      return {
        id: r.key,
        label: r.label,
        value: Number(r.usd),
        display: `${floor ? '≥ ' : ''}${formatReadable(r.usd, true)}`,
        annotation:
          r.unvalued > 0
            ? `${assetsText(r.unvalued)} unvalued`
            : floor
              ? 'lower bound'
              : undefined,
        hatchTail: floor,
        title: !floor
          ? `$${r.usd}`
          : r.unvalued > 0
            ? `At least $${r.usd}. A lower bound: ${assetsText(r.unvalued)} without a reference valuation ${r.unvalued === 1 ? 'is' : 'are'} not counted.`
            : `At least $${r.usd}. A lower bound: the API flags this reference value as partial.`,
      };
    })
    .sort((a, b) => b.value - a.value);
  return (
    <>
      {items.length > 0 && <HBarList items={items} ariaLabel={ariaLabel} />}
      {omitted > 0 && (
        <p className="text-ink-muted mt-1 text-xs">
          {omitted} group{omitted === 1 ? '' : 's'} with no reference valuation
          not shown
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
      <h3 className="text-ink-muted mb-2 text-[11px] font-semibold tracking-wider uppercase">
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
