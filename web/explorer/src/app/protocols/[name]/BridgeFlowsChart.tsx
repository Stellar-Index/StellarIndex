'use client';

import { useMemo, useState } from 'react';
import dynamic from 'next/dynamic';
import { useQuery } from '@tanstack/react-query';

import { Panel } from '@/components/reveal';
import { apiGet, type RequestExample } from '@/api/client';
import { cn } from '@/lib/cn';
import { formatCompact } from '@/lib/format';
import type { NamedLineSeries, LineSeriesTone } from '@/components/charts/LineChart';
import { toChartNumber, type Bespoke, type BespokeSeries } from './BespokeSection';

// BridgeFlowsChart — the bridge protocols' flow chart: ONE chart with the
// inbound + outbound USDC lines (cctp), or the single settled-volume line
// (rozo), with 24h / 7d / 30d / 90d window pills. Switching a pill refetches
// `/v1/protocols/{name}?days=N` client-side; the server buckets the 24h
// window HOURLY (daily otherwise) and keeps the series names direction-
// stable ("Inbound (USDC)" / "Outbound (USDC)" / "Settled volume (USDC)")
// so the pairing below never depends on the window.
//
// Values arrive as numeric STRINGS that can exceed 2^53 (ADR-0003):
// Number() is used ONLY for pixel geometry (the same contract
// TimeSeriesChart documents); displayed figures go through formatCompact.

const LineChart = dynamic(
  () => import('@/components/charts/LineChart').then((m) => m.LineChart),
  { ssr: false, loading: () => <div className="h-56 w-full" /> },
);

const WINDOWS = [
  { key: '24h', days: 1 },
  { key: '7d', days: 7 },
  { key: '30d', days: 30 },
  { key: '90d', days: 90 },
] as const;

type WindowDays = (typeof WINDOWS)[number]['days'];

/** The page's initial fetch covers the server default window (90d). */
const DEFAULT_DAYS: WindowDays = 90;

// Direction pairing is by series NAME — the server keeps these stable per
// direction across every window.
function isInbound(s: BespokeSeries): boolean {
  return /inbound/i.test(s.name);
}
function isOutbound(s: BespokeSeries): boolean {
  return /outbound/i.test(s.name);
}

// pointTime — bespoke point date → unix seconds. Daily points are
// "YYYY-MM-DD"; hourly points (the 24h window) are "YYYY-MM-DDTHH:MM".
function pointTime(date: string): number {
  const iso =
    date.length === 10 ? `${date}T00:00:00Z` : date.length === 16 ? `${date}:00Z` : date;
  const ms = Date.parse(iso);
  return Number.isFinite(ms) ? Math.floor(ms / 1000) : 0;
}

function toLine(s: BespokeSeries, label: string, tone: LineSeriesTone): NamedLineSeries {
  return {
    label,
    tone,
    data: s.points.map((p) => ({ time: pointTime(p.date), value: toChartNumber(p.value) })),
  };
}

// flowLines — project a bespoke series list onto the chart's named lines:
// the inbound/outbound pair when both are present (cctp), else the first
// series as a single line (rozo's settled volume).
export function flowLines(series: BespokeSeries[]): NamedLineSeries[] {
  const inbound = series.find(isInbound);
  const outbound = series.find(isOutbound);
  if (inbound && outbound) {
    return [toLine(inbound, inbound.name, 'up'), toLine(outbound, outbound.name, 'brand')];
  }
  const single = series[0];
  return single ? [toLine(single, single.name, 'brand')] : [];
}

export function BridgeFlowsChart({
  name,
  initial,
  source,
}: {
  /** Protocol slug ("cctp" / "rozo") for the window refetch. */
  name: string;
  /** The bespoke block from the page's initial (90d) fetch. */
  initial: Bespoke;
  source: RequestExample;
}) {
  const [days, setDays] = useState<WindowDays>(DEFAULT_DAYS);

  // Non-default windows refetch the protocol detail with ?days=N. The
  // default window reuses the series the page already fetched — no
  // duplicate request on first render.
  const { data, isFetching, isError } = useQuery<BespokeSeries[]>({
    queryKey: ['/v1/protocols/{name}', name, 'bespoke-window', days],
    enabled: days !== DEFAULT_DAYS,
    retry: false,
    staleTime: 60_000,
    queryFn: async () => {
      const env = await apiGet<{ data?: { bespoke?: Bespoke } }>(
        `/v1/protocols/${encodeURIComponent(name)}?days=${days}`,
      );
      return env.data?.bespoke?.series ?? [];
    },
  });

  const series = useMemo(
    () => (days === DEFAULT_DAYS ? (initial.series ?? []) : (data ?? [])),
    [days, initial.series, data],
  );
  const lines = useMemo(() => flowLines(series), [series]);
  const dual = lines.length > 1;
  const unit = series[0]?.unit;
  const hasPoints = lines.some((l) => l.data.length > 0);
  const windowLabel = WINDOWS.find((w) => w.days === days)?.key ?? `${days}d`;

  return (
    <Panel
      title={dual ? 'Bridge flows' : (series[0]?.name ?? 'Bridge flows')}
      hint={`${unit ? `Units: ${unit} · ` : ''}${days === 1 ? 'hourly' : 'daily'} buckets · last ${windowLabel}`}
      source={source}
    >
      <div className="space-y-3">
        {/* ── Window pills + legend ── */}
        <div className="flex flex-wrap items-center justify-between gap-2">
          <div role="group" aria-label="Chart window" className="flex gap-1">
            {WINDOWS.map((w) => (
              <button
                key={w.key}
                type="button"
                aria-pressed={days === w.days}
                onClick={() => setDays(w.days)}
                className={cn(
                  'rounded-sm px-2 py-0.5 font-mono text-[11px] focus-visible:outline-hidden focus-visible:ring-2 focus-visible:ring-brand-500/60',
                  days === w.days
                    ? 'bg-brand-100 text-brand-700'
                    : 'text-ink-muted hover:text-ink-body',
                )}
              >
                {w.key}
              </button>
            ))}
          </div>
          {hasPoints && (
            <ul className="flex flex-wrap gap-x-4 gap-y-1 text-xs text-ink-muted">
              {lines.map((l) => (
                <li key={l.label} className="flex items-center gap-1.5">
                  <span
                    aria-hidden
                    className={cn(
                      'inline-block h-2 w-2 rounded-full',
                      l.tone === 'up' ? 'bg-up' : l.tone === 'down' ? 'bg-down' : 'bg-brand-500',
                    )}
                  />
                  <span>{l.label}</span>
                  <span className="font-mono tabular-nums text-ink-body">
                    {formatCompact(l.data.reduce((s, p) => s + p.value, 0))}
                  </span>
                </li>
              ))}
            </ul>
          )}
        </div>

        {/* ── Chart / states ── */}
        {isError ? (
          <p className="py-6 text-center text-sm text-ink-muted">
            Couldn&apos;t load this window — retry, or pick another window.
          </p>
        ) : isFetching ? (
          <div
            aria-hidden
            className="h-56 w-full animate-pulse rounded-md bg-surface-subtle"
            data-testid="bridge-flows-loading"
          />
        ) : !hasPoints ? (
          <p className="py-6 text-center text-sm text-ink-muted">
            No transfers in this window.
          </p>
        ) : (
          <LineChart
            data={[]}
            series={lines}
            height={224}
            timeVisible={days === 1}
            legend={{ valueLabel: lines[0]?.label ?? '', formatValue: formatCompact }}
            ariaLabel={`${dual ? 'Inbound and outbound bridge flow' : (lines[0]?.label ?? 'Flow')} over the last ${windowLabel}${unit ? `, in ${unit}` : ''}.`}
          />
        )}
      </div>
    </Panel>
  );
}
