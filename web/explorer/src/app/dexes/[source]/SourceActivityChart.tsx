'use client';

import dynamic from 'next/dynamic';

const LineChart = dynamic(
  () => import('@/components/charts/LineChart').then((m) => m.LineChart),
  { ssr: false, loading: () => <div className="h-[200px]" /> },
);

export interface ActivityBucket {
  hour: string;
  volume_usd: string;
  trade_count?: number;
}

/**
 * SourceActivityChart — the source's trailing-24h activity, as a
 * two-pane lightweight-charts: the trade COUNT per hour as the line
 * (top), with the USD VOLUME per hour as the histogram (bars) beneath.
 * Replaces the old flat volume-by-hour sparkline. Trade count comes
 * from the /v1/sources sparkline buckets' `trade_count` field; volume
 * from `volume_usd`.
 */
export function SourceActivityChart({
  buckets,
  height = 200,
}: {
  buckets: ActivityBucket[];
  height?: number;
}) {
  const data = buckets.map((b) => ({
    time: Math.floor(new Date(b.hour).getTime() / 1000),
    value: b.trade_count ?? 0, // line: quantity of trades
    volume: Number(b.volume_usd) || 0, // bars: USD volume
  }));
  return (
    <LineChart
      data={data}
      height={height}
      timeVisible
      ariaLabel="Hourly trade count (line) over USD volume (bars) for the trailing 24 hours"
    />
  );
}
