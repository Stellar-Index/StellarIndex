'use client';

import { DonutChart, type DonutSlice } from '@/components/charts/DonutChart';
import { formatCompact, ratioPct } from '@/lib/format';

/**
 * survivalSlices — a cohort split into members still live and members
 * gone. Null when the counts cannot describe one whole (no members, or
 * more live than members), so no remainder is invented.
 */
export function survivalSlices(
  accounts: number,
  live: number,
): DonutSlice[] | null {
  if (!Number.isInteger(accounts) || !Number.isInteger(live)) return null;
  if (accounts <= 0 || live < 0 || live > accounts) return null;
  const gone = accounts - live;
  return [
    {
      id: 'live',
      label: 'Still live',
      value: live,
      decimal: String(live),
      color: 'var(--color-up)',
    },
    {
      id: 'gone',
      label: 'No account entry now',
      value: gone,
      decimal: String(gone),
      color: 'var(--color-ink-faint)',
    },
  ];
}

/** SurvivalDonut — how much of a cohort still has an account entry. */
export function SurvivalDonut({
  accounts,
  live,
}: {
  accounts: number;
  live: number;
}) {
  const slices = survivalSlices(accounts, live);
  if (!slices) return null;
  const pct = ratioPct(String(live), String(accounts), 1);
  return (
    <div data-testid="survival-donut">
      <DonutChart
        data={slices}
        size={120}
        thickness={18}
        centerLabel={pct == null ? undefined : `${pct}%`}
        centerSub="still live"
        formatValue={formatCompact}
      />
    </div>
  );
}
