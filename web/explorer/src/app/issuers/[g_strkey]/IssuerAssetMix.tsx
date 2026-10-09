'use client';

import { DonutChart } from '@/components/charts/DonutChart';
import { topSlices } from '@/components/charts/topSlices';
import {
  formatCompact,
  formatCompactUnits,
  sumDecimalStrings,
} from '@/lib/format';

/**
 * Issued assets by USD market cap. Assets without a served market cap are
 * counted and named, so the total reads as the lower bound it is.
 */
export function IssuerAssetMix({
  assets,
}: {
  assets: {
    id: string;
    code: string;
    href: string;
    marketCapUsd?: string | null;
  }[];
}) {
  const priced = assets.flatMap((a) => {
    const n = Number(a.marketCapUsd ?? '');
    return a.marketCapUsd != null && Number.isFinite(n) && n > 0
      ? [
          {
            id: a.id,
            label: a.code,
            href: a.href,
            value: n,
            decimal: a.marketCapUsd,
          },
        ]
      : [];
  });
  if (priced.length < 2) return null;
  const unpriced = assets.length - priced.length;
  const total = sumDecimalStrings(priced.map((p) => p.decimal));
  return (
    <div className="px-4 pb-4">
      <div className="text-ink-muted mb-2 text-[11px] tracking-wider uppercase">
        Market cap by asset
        {unpriced > 0 && ` · ${unpriced} without a market cap excluded`}
      </div>
      <DonutChart
        data={topSlices(priced, 6)}
        size={130}
        thickness={18}
        centerLabel={
          total
            ? `${unpriced > 0 ? '≥ ' : ''}$${formatCompactUnits(total, 0)}`
            : undefined
        }
        centerSub={unpriced > 0 ? 'priced market cap' : 'market cap'}
        formatValue={(n) => `$${formatCompact(n)}`}
      />
    </div>
  );
}
