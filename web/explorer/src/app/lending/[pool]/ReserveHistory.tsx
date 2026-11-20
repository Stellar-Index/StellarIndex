'use client';

import { useMemo, useState } from 'react';
import { useQuery } from '@tanstack/react-query';

import { Button, Segmented } from '@/components/ui';
import { apiGet } from '@/api/client';
import { FreshnessMarker } from '@/components/primitives';
import type { paths } from '@/api/types';
import { formatCompact, formatRelative } from '@/lib/format';
import {
  carryForward,
  configChanges,
  isHistoryBuilding,
  metricValue,
  METRICS,
  RANGES,
  type HistoryMetric,
  type HistoryView,
  type RangeOption,
} from '../lendingInsights';
import { isUnlimitedCap } from '../poolConfig';

const APR_COLORS = {
  supply: 'var(--color-chart-2)',
  borrow: 'var(--color-chart-4)',
};
import { HistoryChart } from './HistoryChart';

type Resp =
  paths['/lending/pools/{pool}/reserves/{asset}/history']['get']['responses'][200]['content']['application/json'];

function useReserveHistory(pool: string, asset: string, range: RangeOption) {
  return useQuery({
    queryKey: [
      '/v1/lending/pools/{pool}/reserves/{asset}/history',
      pool,
      asset,
      range.id,
    ],
    retry: false,
    staleTime: 60_000,
    queryFn: async () => {
      const env = await apiGet<Resp>(
        `/v1/lending/pools/${encodeURIComponent(pool)}/reserves/${encodeURIComponent(asset)}/history`,
        { interval: range.interval, window: range.window },
      );
      return env.data ? { ...env.data, flags: env.flags } : env.data;
    },
  });
}

/** One reserve's history chart; mounted (and so fetched) only when its row is opened. */
export function ReserveHistory({
  pool,
  asset,
}: {
  pool: string;
  asset: string;
}) {
  const [metric, setMetric] = useState<HistoryView>('borrow_apr');
  const [rangeId, setRangeId] = useState<RangeOption['id']>('90d');
  const range = RANGES.find((r) => r.id === rangeId) ?? RANGES[1];
  const q = useReserveHistory(pool, asset, range);
  const meta = METRICS.find((m) => m.id === metric) ?? METRICS[0];
  const d = q.data;

  const { series, overlay } = useMemo(() => {
    if (!d) return { series: [], overlay: undefined };
    const endIso =
      d.history_through_time && d.history_through_time < d.to
        ? d.history_through_time
        : d.to;
    const of = (m: HistoryMetric) =>
      carryForward(
        d.points.map((p) => ({
          t: p.t,
          value: metricValue(p, m, d.decimals, d.config_decimals),
        })),
        range.interval,
        d.from,
        endIso,
      );
    if (metric !== 'aprs') return { series: of(metric), overlay: undefined };
    return {
      series: [],
      overlay: [
        {
          label: 'Supply APR',
          points: of('supply_apr'),
          color: APR_COLORS.supply,
        },
        {
          label: 'Borrow APR',
          points: of('borrow_apr'),
          color: APR_COLORS.borrow,
        },
      ],
    };
  }, [d, metric, range.interval]);
  const capUnlimited =
    metric === 'supply_cap' &&
    (d?.points ?? []).some((p) => isUnlimitedCap(p.supply_cap));

  const changes = useMemo(
    () =>
      d ? configChanges(d.points, d.config_decimals, d.decimals).reverse() : [],
    [d],
  );

  return (
    <div className="space-y-3 md:col-span-2">
      <h3 className="text-ink text-sm font-medium">History</h3>
      <div className="flex flex-wrap gap-x-6 gap-y-2">
        <Segmented
          ariaLabel="Metric"
          className="max-w-full overflow-x-auto"
          options={METRICS.map((m) => ({ label: m.label, value: m.id }))}
          value={metric}
          onChange={(v) => setMetric(v as HistoryView)}
        />
        <Segmented
          ariaLabel="Range"
          options={RANGES.map((r) => ({ label: r.label, value: r.id }))}
          value={rangeId}
          onChange={(v) => setRangeId(v as RangeOption['id'])}
        />
      </div>
      {q.isLoading && (
        <p className="text-ink-muted text-sm">Loading history…</p>
      )}
      {q.isError && isHistoryBuilding(q.error) && (
        <p className="text-ink-muted text-sm">
          History is still being built for this reserve. Check back soon.
        </p>
      )}
      {q.isError && !isHistoryBuilding(q.error) && (
        <p className="text-ink-muted text-sm">
          History is unavailable right now.
          <Button
            variant="secondary"
            size="sm"
            className="ml-3"
            onClick={() => void q.refetch()}
          >
            Retry
          </Button>
        </p>
      )}
      {d && (
        <>
          <FreshnessMarker flags={d.flags} className="block" />
          {d.dropped_points > 0 && (
            <p className="text-ink-muted text-xs">
              {d.dropped_points.toLocaleString('en-US')} point
              {d.dropped_points === 1 ? '' : 's'} could not be decoded and{' '}
              {d.dropped_points === 1 ? 'is' : 'are'} missing; the chart holds
              the previous value across {d.dropped_points === 1 ? 'it' : 'them'}
              .
            </p>
          )}
          {d.before_first_state && (
            <p className="text-ink-muted text-xs">
              This reserve has no recorded state before the first point shown;
              earlier time is blank.
            </p>
          )}
          {d.points.length === 0 ? (
            <p className="text-ink-muted text-sm">
              No state changes recorded in this range.
            </p>
          ) : (
            <HistoryChart
              points={series}
              overlay={overlay}
              label={meta.label}
              pct={meta.isPct}
              hourly={range.interval === '1h'}
              formatValue={(n) =>
                meta.isPct ? `${n.toFixed(2)}%` : formatCompact(n)
              }
            />
          )}
          {capUnlimited && (
            <p className="text-ink-muted text-xs">
              Gaps in the supply cap line are periods with no cap.
            </p>
          )}
          <p className="text-ink-muted text-xs">
            Each value holds until the next change.
            {d.history_through_time && (
              <>
                {' '}
                Data through{' '}
                <span title={d.history_through_time}>
                  {d.history_through_time.slice(0, 16).replace('T', ' ')} UTC (
                  {formatRelative(d.history_through_time)})
                </span>
                .
              </>
            )}
          </p>
          {changes.length > 0 && (
            <div className="space-y-1">
              <h4 className="text-ink text-xs font-medium">
                Configuration changes in this range
              </h4>
              <ul className="text-ink-body space-y-0.5 text-xs">
                {changes.map((c) => (
                  <li key={`${c.at}-${c.label}`}>
                    {c.label} <span className="font-mono">{c.from}</span> →{' '}
                    <span className="font-mono">{c.to}</span> on{' '}
                    <span title={c.at}>{c.at.slice(0, 10)}</span>
                  </li>
                ))}
              </ul>
            </div>
          )}
        </>
      )}
    </div>
  );
}
