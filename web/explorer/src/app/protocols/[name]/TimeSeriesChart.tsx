'use client';

import { useMemo } from 'react';
import dynamic from 'next/dynamic';

import { formatCompact } from '@/lib/format';
import { isPartialTodayDate, seriesPointTime } from '@/lib/series';

// TimeSeriesChart — one measured series (on-chain activity, every bespoke
// `series` entry) on the shared LineChart, with a peak / average / latest
// summary. `value` is a JS number used ONLY for chart geometry: the bespoke
// wire ships numeric strings that can exceed 2^53 (ADR-0003), the caller
// parses them for the shape, and the summary shows formatCompact of that
// shape — a cosmetic loss in a pixel coordinate, never a served amount.

const LineChart = dynamic(
  () => import('@/components/charts/LineChart').then((m) => m.LineChart),
  { ssr: false, loading: () => <div className="h-56 w-full" /> },
);

export interface ChartPoint {
  date: string; // YYYY-MM-DD, or YYYY-MM-DDTHH:MM for hourly (24h-window) series
  value: number;
}

export function TimeSeriesChart({
  points,
  label,
  unit,
  timeVisible,
}: {
  /** The series to plot; `value` is already a JS number (geometry only). */
  points: ChartPoint[];
  /** Human label for the legend and aria summary (e.g. "USD volume"). */
  label: string;
  /** Optional unit appended to the summary figures (e.g. "USD", "events"). */
  unit?: string;
  /** Show time-of-day on the x-axis (hourly 24h-window series). */
  timeVisible?: boolean;
}) {
  // Points with a non-parsable date are dropped before setData — a time-0
  // fallback plotted them at 1970 and two of them fed lightweight-charts
  // duplicate times (an ascending-order crash).
  const linePoints = useMemo(
    () =>
      points.flatMap((p) => {
        const time = seriesPointTime(p.date);
        return time == null ? [] : [{ time, value: p.value }];
      }),
    [points],
  );

  if (points.length === 0) {
    return (
      <p className="text-ink-muted py-6 text-center text-sm">
        No data points in the window.
      </p>
    );
  }

  const unitSuffix = unit ? ` ${unit}` : '';
  const peak = points.reduce(
    (best, p) => (p.value > best.value ? p : best),
    points[0],
  );
  const total = points.reduce((s, p) => s + p.value, 0);
  const avg = total / points.length;
  // "Latest" headlines the last COMPLETE point: callers drop today's
  // accumulating daily bucket upstream (dropPartialTrailingDay), and this
  // guard keeps the headline honest even for a caller that didn't.
  const lastComplete =
    [...points].reverse().find((p) => !isPartialTodayDate(p.date)) ?? null;
  // A whole-number series (counts) gets a whole-number axis.
  const integral = points.every((p) => Number.isInteger(p.value));
  const ariaLabel = `${label}: ${points.length} points, peak ${formatCompact(peak.value)}${unitSuffix} on ${peak.date}, average ${formatCompact(Math.round(avg))}${unitSuffix} over the window.`;

  return (
    <div className="space-y-3">
      <div className="text-ink-muted flex flex-wrap items-baseline gap-x-6 gap-y-1 text-xs">
        <Figure
          label="Peak"
          value={`${formatCompact(peak.value)}${unitSuffix}`}
          note={`on ${peak.date}`}
        />
        <Figure
          label={timeVisible ? 'Avg per hour' : 'Avg per day'}
          value={`${formatCompact(Math.round(avg))}${unitSuffix}`}
        />
        <Figure
          label="Latest"
          value={
            lastComplete != null
              ? `${formatCompact(lastComplete.value)}${unitSuffix}`
              : '—'
          }
        />
      </div>
      <LineChart
        data={linePoints}
        height={224}
        tone="brand"
        area={false}
        priceFormat={{ type: 'volume', precision: integral ? 0 : 2 }}
        legend={{ valueLabel: label, formatValue: formatCompact }}
        ariaLabel={ariaLabel}
        timeVisible={timeVisible}
      />
    </div>
  );
}

function Figure({
  label,
  value,
  note,
}: {
  label: string;
  value: string;
  note?: string;
}) {
  return (
    <span>
      {label} <span className="text-ink tnum font-mono">{value}</span>
      {note ? ` ${note}` : ''}
    </span>
  );
}
