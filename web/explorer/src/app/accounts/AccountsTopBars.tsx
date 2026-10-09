'use client';

import { HBarList } from '@/components/charts/Bars';
import {
  formatCompactUnits,
  ratioPct,
  sumDecimalStrings,
  truncateMiddle,
} from '@/lib/format';

const TOP_N = 10;

/**
 * Ranked bars for the directory's top accounts. Shares are of the LISTED
 * accounts' total (the served page), not of the network.
 */
export function AccountsTopBars({
  rows,
  isNative,
}: {
  rows: { account_id: string; value: string }[];
  isNative: boolean;
}) {
  const listedTotal = sumDecimalStrings(rows.map((r) => r.value));
  const top = rows.slice(0, TOP_N);
  const items = top.flatMap((r) => {
    const n = Number(r.value);
    const amount = formatCompactUnits(r.value, 0);
    if (!Number.isFinite(n) || amount === '—') return [];
    const share = ratioPct(r.value, listedTotal, 1);
    return [
      {
        id: r.account_id,
        label: truncateMiddle(r.account_id, 6, 6),
        title: r.account_id,
        value: n,
        display: isNative ? `${amount} XLM` : `$${amount}`,
        annotation: share != null ? `${share.toFixed(1)}%` : undefined,
      },
    ];
  });
  if (items.length === 0) return null;
  return (
    <div>
      <div className="text-ink-muted mb-2 text-[11px] tracking-wider uppercase">
        Top {items.length} of {rows.length} listed accounts · % of listed total
      </div>
      <HBarList
        ariaLabel={`Top ${items.length} of ${rows.length} listed accounts by ${isNative ? 'XLM balance' : 'USD value'}`}
        items={items}
      />
    </div>
  );
}
