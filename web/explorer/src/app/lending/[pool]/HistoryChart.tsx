'use client';

import dynamic from 'next/dynamic';

import type { StepPoint } from '../lendingInsights';

const LineChart = dynamic(
  () => import('@/components/charts/LineChart').then((m) => m.LineChart),
  { ssr: false, loading: () => <div className="h-56 w-full" /> },
);

/** A held-value series on the shared line chart; null values leave a gap. */
export function HistoryChart({
  points,
  label,
  formatValue,
  hourly,
  pct,
}: {
  points: StepPoint[];
  label: string;
  formatValue: (n: number) => string;
  hourly?: boolean;
  pct?: boolean;
}) {
  if (points.every((p) => p.value == null)) {
    return (
      <p className="text-ink-muted py-6 text-center text-sm">
        No data points in this range.
      </p>
    );
  }
  return (
    <LineChart
      data={points}
      height={224}
      tone="brand"
      area={false}
      priceFormat={
        pct
          ? { type: 'price', precision: 2, minMove: 0.01 }
          : { type: 'volume', precision: 2 }
      }
      legend={{ valueLabel: label, formatValue }}
      ariaLabel={`${label} over time, ${points.length} points`}
      timeVisible={hourly}
    />
  );
}
