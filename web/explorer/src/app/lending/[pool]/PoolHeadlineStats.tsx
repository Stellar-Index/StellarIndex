'use client';

import { Button, Skeleton, Stat, StatCell, StatGrid } from '@/components/ui';

import { useLendingPools } from '../LendingPoolsTable';
import { UsdFigure } from '../lendingUsd';

const pct = (f: number | null | undefined) =>
  f == null ? '—' : `${(f * 100).toFixed(2)}%`;

/**
 * PoolHeadlineStats — the pool's live TVL, borrowed, utilisation and
 * weighted APRs from its /v1/lending/pools summary (priced reserves only).
 */
export function PoolHeadlineStats({ pool }: { pool: string }) {
  const q = useLendingPools();
  if (q.isLoading) return <Skeleton className="h-20 w-full" />;
  if (q.isError)
    return (
      <p role="alert" className="text-ink-muted text-sm">
        Pool totals are unavailable right now.
        <Button
          variant="secondary"
          size="sm"
          className="ml-3"
          onClick={() => void q.refetch()}
        >
          Retry
        </Button>
      </p>
    );
  const s = q.data?.rows.find((r) => r.pool === pool)?.summary;
  const basis =
    s && (s.reserve_count ?? 0) > (s.priced_reserve_count ?? 0)
      ? `priced reserves only (${s.priced_reserve_count ?? 0} of ${s.reserve_count})`
      : 'priced reserves only';
  return (
    <StatGrid cols={5} className="grid-cols-2">
      <StatCell>
        <Stat
          label="TVL"
          value={
            <UsdFigure
              value={s?.tvl_usd}
              lowerBound={s?.lower_bound === true}
              reason={s ? basis : 'pool summary not served'}
            />
          }
          sub="Supplied, USD"
        />
      </StatCell>
      <StatCell>
        <Stat
          label="Borrowed"
          value={
            <UsdFigure
              value={s?.borrowed_usd}
              lowerBound={s?.borrowed_lower_bound === true}
              reason={s ? basis : 'pool summary not served'}
            />
          }
          sub="USD"
        />
      </StatCell>
      <StatCell>
        <Stat
          label="Utilisation"
          value={
            s?.utilization_pct != null
              ? `${s.utilization_pct.toFixed(1)}%`
              : '—'
          }
          sub="Borrowed / supplied"
        />
      </StatCell>
      <StatCell>
        <Stat label="Supply APR" value={pct(s?.supply_apr)} sub="Weighted" />
      </StatCell>
      <StatCell>
        <Stat label="Borrow APR" value={pct(s?.borrow_apr)} sub="Weighted" />
      </StatCell>
    </StatGrid>
  );
}
