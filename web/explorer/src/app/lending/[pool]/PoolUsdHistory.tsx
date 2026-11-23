'use client';

import { useState } from 'react';
import { useQuery } from '@tanstack/react-query';

import { Segmented } from '@/components/ui';
import { apiGet } from '@/api/client';
import { FreshnessMarker } from '@/components/primitives';
import type { paths } from '@/api/types';
import { formatCompact } from '@/lib/format';
import {
  isHistoryBuilding,
  RANGES,
  type RangeOption,
  type StepPoint,
} from '../lendingInsights';
import { useAssetText } from '../lendingUsd';
import { HistoryChart } from './HistoryChart';

type Resp =
  paths['/lending/pools/{pool}/history']['get']['responses'][200]['content']['application/json'];
export type PoolUsdPoint = NonNullable<Resp['data']>['points'][number];

const COLORS = {
  supplied: 'var(--color-chart-2)',
  borrowed: 'var(--color-chart-4)',
};

export interface UsdSeries {
  supplied: StepPoint[];
  borrowed: StepPoint[];
  /** Buckets whose supplied figure is a lower bound. */
  lowerBound: number;
  /** Reserves some bucket could not value, in first-seen order. */
  leftOut: string[];
}

const usd = (s: string | null | undefined): number | null =>
  s == null ? null : Number(s);

/**
 * The pool's TVL and borrowed USD per bucket, or one reserve's when `asset` is set. An unvalued bucket is a null (a gap),
 * never 0; a reserve absent from a bucket is a gap too.
 */
export function usdSeries(
  points: PoolUsdPoint[],
  omitted: string[],
  asset?: string,
): UsdSeries {
  const out: UsdSeries = {
    supplied: [],
    borrowed: [],
    lowerBound: 0,
    leftOut: [...omitted],
  };
  const note = (a: string) => {
    if (!out.leftOut.includes(a)) out.leftOut.push(a);
  };
  for (const p of points) {
    const time = Math.floor(Date.parse(p.t) / 1000);
    if (asset) {
      const r = p.reserves.find((x) => x.asset === asset);
      out.supplied.push({ time, value: usd(r?.supplied_usd) });
      out.borrowed.push({ time, value: usd(r?.borrowed_usd) });
      continue;
    }
    out.supplied.push({ time, value: usd(p.tvl_usd) });
    out.borrowed.push({ time, value: usd(p.borrowed_usd) });
    if (p.lower_bound) out.lowerBound++;
    p.unvalued_reserves.forEach(note);
  }
  if (asset) out.leftOut = [];
  return out;
}

/** Supplied (TVL) and borrowed in USD over time, each bucket valued at its own price. */
export function PoolUsdHistory({
  pool,
  asset,
}: {
  pool: string;
  asset?: string;
}) {
  const [rangeId, setRangeId] = useState<RangeOption['id']>('90d');
  const range = RANGES.find((r) => r.id === rangeId) ?? RANGES[1];
  const text = useAssetText();
  const q = useQuery({
    queryKey: ['/v1/lending/pools/{pool}/history', pool, range.id],
    retry: false,
    staleTime: 60_000,
    queryFn: async () => {
      const env = await apiGet<Resp>(
        `/v1/lending/pools/${encodeURIComponent(pool)}/history`,
        {
          interval: range.interval,
          window: range.window,
        },
      );
      return env.data ? { ...env.data, flags: env.flags } : env.data;
    },
  });
  const d = q.data;
  const s = d ? usdSeries(d.points, d.omitted_reserves, asset) : null;
  const suppliedLabel = asset
    ? 'Supplied (USD)'
    : s && s.lowerBound > 0
      ? 'TVL (USD, ≥)'
      : 'TVL (USD)';

  return (
    <div className="space-y-3 md:col-span-2">
      <h3 className="text-ink text-sm font-medium">
        {asset ? 'Value over time' : 'Pool value over time'}
      </h3>
      <Segmented
        ariaLabel="Range"
        options={RANGES.map((r) => ({ label: r.label, value: r.id }))}
        value={rangeId}
        onChange={(v) => setRangeId(v as RangeOption['id'])}
      />
      {q.isLoading && (
        <p className="text-ink-muted text-sm">Loading value history…</p>
      )}
      {q.isError && (
        <p className="text-ink-muted text-sm">
          {isHistoryBuilding(q.error)
            ? 'Value history is still being built for this pool. Check back soon.'
            : 'Value history is unavailable right now.'}
        </p>
      )}
      {d && s && (
        <>
          <FreshnessMarker flags={d.flags} className="block" />
          <HistoryChart
            points={s.supplied}
            overlay={[
              {
                label: suppliedLabel,
                points: s.supplied,
                color: COLORS.supplied,
              },
              {
                label: 'Borrowed (USD)',
                points: s.borrowed,
                color: COLORS.borrowed,
              },
            ]}
            label={suppliedLabel}
            hourly={range.interval === '1h'}
            formatValue={(n) => `$${formatCompact(n)}`}
          />
          {s.lowerBound > 0 && (
            <p className="text-ink-muted text-xs">
              ≥ {s.lowerBound.toLocaleString('en-US')} of{' '}
              {d.points.length.toLocaleString('en-US')} points are lower bounds:
              they leave out{' '}
              {s.leftOut.map((a, i) => (
                <span key={a}>
                  {i > 0 && ', '}
                  <span title={a}>{text(a)}</span>
                </span>
              ))}
              .
            </p>
          )}
          <p className="text-ink-muted text-xs">
            Each point is valued at that{' '}
            {range.interval === '1h' ? 'hour' : 'day'}&apos;s USD price, at most{' '}
            {d.price_max_age_hours} hours old; gaps are points with no price,
            never zero.
          </p>
        </>
      )}
    </div>
  );
}
