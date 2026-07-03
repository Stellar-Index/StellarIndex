'use client';

import { useState } from 'react';
import dynamic from 'next/dynamic';
import Link from 'next/link';
import { useQuery } from '@tanstack/react-query';

import { Panel } from '@/components/reveal';
import { apiGet, asExample } from '@/api/client';
import { EmptyState, Skeleton } from '@/components/ui';
import type { paths } from '@/api/types';
import { formatCompact } from '@/lib/format';
import type { Envelope } from '@/app/explorer-shared';

const LineChart = dynamic(
  () => import('@/components/charts/LineChart').then((m) => m.LineChart),
  { ssr: false, loading: () => <div className="h-[240px]" /> },
);

// Wire shapes derived from the generated OpenAPI contract.
type ThroughputResp = NonNullable<
  paths['/network/throughput']['get']['responses'][200]['content']['application/json']['data']
>;
type OperationsResp = NonNullable<
  paths['/operations']['get']['responses'][200]['content']['application/json']['data']
>;
export type OpTypeStat = NonNullable<OperationsResp['op_type_stats']>[number];

export type ThroughputMetric = 'ops' | 'txs' | 'events' | 'ledgers';

/**
 * Shared insight panels for the network-directory pages (site audit
 * S-004/S-005: /transactions, /operations and /ledgers were bare
 * paginated lists while the API already served the aggregates —
 * only /network consumed them). Each panel owns its fetch so a
 * directory page adds insight with one JSX line.
 */

/** useOpTypeStats — the trailing-24h per-op-type counts. */
export function useOpTypeStats() {
  return useQuery<OpTypeStat[]>({
    queryKey: ['/v1/operations', 'type-mix'],
    queryFn: async () =>
      (await apiGet<Envelope<OperationsResp>>('/v1/operations', { limit: 1 })).data
        .op_type_stats ?? [],
    staleTime: 60_000,
    retry: false,
  });
}

/**
 * OperationMixPanel — trailing-24h op-type distribution as ranked
 * bars. `linkRows` controls whether rows deep-link to /operations
 * (turn it off ON /operations itself).
 */
export function OperationMixPanel({ linkRows = true }: { linkRows?: boolean }) {
  const { data: stats, isLoading, isError } = useOpTypeStats();
  const sorted = [...(stats ?? [])]
    .sort((a, b) => (b.count ?? 0) - (a.count ?? 0))
    .slice(0, 10);
  const max = sorted.reduce((m, x) => Math.max(m, x.count ?? 0), 0) || 1;
  const grand = (stats ?? []).reduce((sum, x) => sum + (x.count ?? 0), 0) || 1;

  return (
    <Panel
      title="Operation mix — trailing 24h"
      source={asExample('/v1/operations', { limit: 1 })}
      bodyClassName="space-y-2.5"
    >
      {isLoading && <Skeleton className="h-40 w-full" />}
      {isError && (
        <p className="text-sm text-ink-muted">Operation stats are unavailable right now.</p>
      )}
      {!isLoading && !isError && sorted.length === 0 && (
        <EmptyState title="No operations in the last 24h." />
      )}
      {sorted.map((st) => {
        const count = st.count ?? 0;
        const pct = (count / grand) * 100;
        const row = (
          <>
            <div className="mb-0.5 flex items-baseline justify-between gap-2 text-xs">
              <code className="truncate text-ink-body group-hover:text-brand-600">
                {st.type}
              </code>
              <span className="shrink-0 font-mono tabular-nums text-ink-muted">
                {formatCompact(count)}
                <span className="ml-1 text-ink-faint">{pct.toFixed(1)}%</span>
              </span>
            </div>
            <div className="h-1.5 overflow-hidden rounded-full bg-surface-muted">
              <div
                className="h-full rounded-full bg-brand-500 transition-all group-hover:bg-brand-600"
                style={{ width: `${(count / max) * 100}%` }}
              />
            </div>
          </>
        );
        return linkRows ? (
          <Link
            key={st.type}
            href="/operations"
            className="group block"
            title={`${count.toLocaleString()} ${st.type} ops (${pct.toFixed(1)}%)`}
          >
            {row}
          </Link>
        ) : (
          <div
            key={st.type}
            className="group"
            title={`${count.toLocaleString()} ${st.type} ops (${pct.toFixed(1)}%)`}
          >
            {row}
          </div>
        );
      })}
    </Panel>
  );
}

/**
 * ThroughputPanel — daily network throughput as a line chart with a
 * metric selector. `defaultMetric` picks the series that matches the
 * hosting page (txs on /transactions, ops on /operations, ledgers on
 * /ledgers) so the first paint answers that page's question.
 */
export function ThroughputPanel({
  defaultMetric = 'ops',
  windowDays = 30,
  height = 240,
}: {
  defaultMetric?: ThroughputMetric;
  windowDays?: number;
  height?: number;
}) {
  const [metric, setMetric] = useState<ThroughputMetric>(defaultMetric);
  const tpQ = useQuery<ThroughputResp>({
    queryKey: ['/v1/network/throughput', windowDays],
    queryFn: async () =>
      (
        await apiGet<Envelope<ThroughputResp>>('/v1/network/throughput', {
          window_days: windowDays,
        })
      ).data,
    staleTime: 60_000,
  });

  const buckets = tpQ.data?.buckets ?? [];
  const points = buckets.map((b) => ({
    time: Math.floor(Date.parse(`${b.day ?? ''}T00:00:00Z`) / 1000),
    value: b[metric] ?? 0,
  }));
  const total = buckets.reduce((s, b) => s + (b[metric] ?? 0), 0);
  const labels: Record<ThroughputMetric, string> = {
    ops: 'Operations',
    txs: 'Transactions',
    events: 'Contract events',
    ledgers: 'Ledgers',
  };

  return (
    <Panel
      title={`${labels[metric]} per day — trailing ${windowDays}d`}
      hint={total > 0 ? `${formatCompact(total)} total` : undefined}
      source={asExample('/v1/network/throughput', { window_days: windowDays })}
      bodyClassName="space-y-3"
    >
      <div className="flex flex-wrap gap-1">
        {(Object.keys(labels) as ThroughputMetric[]).map((m) => (
          <button
            key={m}
            onClick={() => setMetric(m)}
            className={`rounded-md px-2.5 py-1 text-xs ${
              metric === m
                ? 'bg-brand-600 text-white'
                : 'border border-line text-ink-body hover:border-brand-500'
            }`}
          >
            {labels[m]}
          </button>
        ))}
      </div>
      {tpQ.isLoading ? (
        <Skeleton className="h-60 w-full" />
      ) : points.length === 0 ? (
        <EmptyState title="No throughput data for this window." />
      ) : (
        <LineChart
          data={points}
          height={height}
          positive
          area
          timeVisible
          ariaLabel={`Daily ${labels[metric].toLowerCase()} over the trailing ${windowDays} days`}
          legend={{ valueLabel: labels[metric], formatValue: formatCompact }}
        />
      )}
    </Panel>
  );
}
