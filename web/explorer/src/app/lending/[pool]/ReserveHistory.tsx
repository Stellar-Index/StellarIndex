'use client';

import { useMemo, useState } from 'react';
import { useQuery } from '@tanstack/react-query';

import { Button, Segmented } from '@/components/ui';
import { apiGet } from '@/api/client';
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
  type RangeOption,
} from '../lendingInsights';
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
      return env.data;
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
  const [metric, setMetric] = useState<HistoryMetric>('borrow_apr');
  const [rangeId, setRangeId] = useState<RangeOption['id']>('90d');
  const range = RANGES.find((r) => r.id === rangeId) ?? RANGES[1];
  const q = useReserveHistory(pool, asset, range);
  const meta = METRICS.find((m) => m.id === metric) ?? METRICS[0];
  const d = q.data;

  const series = useMemo(() => {
    if (!d) return [];
    const endIso =
      d.history_through_time && d.history_through_time < d.to
        ? d.history_through_time
        : d.to;
    return carryForward(
      d.points.map((p) => ({
        t: p.t,
        value: metricValue(p, metric, d.decimals),
      })),
      range.interval,
      d.from,
      endIso,
    );
  }, [d, metric, range.interval]);

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
          options={METRICS.map((m) => ({ label: m.label, value: m.id }))}
          value={metric}
          onChange={(v) => setMetric(v as HistoryMetric)}
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
              label={meta.label}
              pct={meta.isPct}
              hourly={range.interval === '1h'}
              formatValue={(n) =>
                meta.isPct ? `${n.toFixed(2)}%` : formatCompact(n)
              }
            />
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
