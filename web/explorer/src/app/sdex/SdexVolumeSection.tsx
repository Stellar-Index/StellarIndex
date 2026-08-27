'use client';

import dynamic from 'next/dynamic';
import { useQuery } from '@tanstack/react-query';

import { Panel } from '@/components/reveal';
import { apiGet, asExample } from '@/api/client';
import { CURRENT_NETWORK } from '@/lib/networks';
import { formatCompact } from '@/lib/format';
import { dropPartialTrailingDay } from '@/lib/series';

// Lazy-load lightweight-charts (~155 KB) — only this section needs it.
const VolumeLineChart = dynamic(
  () => import('@/components/charts/LineChart').then((m) => m.LineChart),
  { ssr: false, loading: () => <div className="h-[240px]" /> },
);

interface BespokeSeriesPoint {
  date: string;
  value: string;
}
interface BespokeSeries {
  name: string;
  unit: string;
  points: BespokeSeriesPoint[];
}
interface BespokeKpi {
  label: string;
  value: string;
  unit?: string | null;
}
interface ProtocolDetail {
  name: string;
  events_24h?: number;
  bespoke?: {
    kpis?: BespokeKpi[];
    series?: BespokeSeries[];
  };
}

/**
 * SdexVolumeSection — daily SDEX USD volume + headline KPIs from the
 * protocol analytics endpoint (GET /v1/protocols/sdex). Series lookup
 * is by the backend's exact emitted name ("USD volume",
 * internal/api/v1/bespoke_dex.go) — an absent or renamed series renders
 * an honest unavailable note, never an empty chart. USD figures are
 * ingest-time trade valuations only; unpriced trades are excluded from
 * USD sums (the endpoint's own methodology note).
 */
export function SdexVolumeSection() {
  const q = useQuery<ProtocolDetail>({
    queryKey: ['/v1/protocols', 'sdex'],
    queryFn: async () =>
      (await apiGet<{ data: ProtocolDetail }>('/v1/protocols/sdex')).data,
    staleTime: 5 * 60_000,
    retry: false,
  });

  const series = q.data?.bespoke?.series?.find((s) => s.name === 'USD volume');
  // Today's accumulating UTC bucket is dropped (it would plot as a phantom
  // cliff); non-parsable points are dropped rather than plotted at 1970/NaN.
  const points = dropPartialTrailingDay(series?.points ?? [])
    .map((pt) => ({
      date: pt.date,
      time: Math.floor(Date.parse(`${pt.date}T00:00:00Z`) / 1000),
      value: Number(pt.value),
    }))
    .filter((pt) => Number.isFinite(pt.time) && Number.isFinite(pt.value));

  // On the lean nets there is no aggregator, so every trade is unpriced and the
  // USD sums are all 0. Drop the USD-denominated KPIs (they'd read a misleading
  // "$0") and the USD-volume chart, keeping the trade/pair counts, which are
  // real. Mainnet is unchanged.
  const priced = CURRENT_NETWORK.pricing;
  const kpis = (q.data?.bespoke?.kpis ?? [])
    .filter((k) => priced || k.unit !== 'USD')
    .slice(0, 3);

  return (
    <Panel
      title={priced ? 'SDEX volume' : 'SDEX activity'}
      hint={
        priced
          ? 'Daily USD volume of priced SDEX trades — unpriced trades count toward trade totals but not USD sums.'
          : 'Trade and pair counts from the SDEX order book. USD volume needs the pricing layer, which this network does not run.'
      }
      source={asExample('/v1/protocols/sdex')}
      bodyClassName="space-y-4"
    >
      {kpis.length > 0 && (
        <dl className="grid grid-cols-1 gap-3 text-sm sm:grid-cols-3">
          {kpis.map((k) => (
            <div key={k.label}>
              <dt className="text-[11px] uppercase tracking-wider text-ink-muted">
                {k.label}
              </dt>
              <dd className="mt-0.5 font-mono text-lg tabular-nums text-ink">
                {formatKpi(k)}
              </dd>
            </div>
          ))}
        </dl>
      )}
      {/* The chart is USD-denominated — only rendered where pricing exists. */}
      {priced && (
        <>
          {q.isLoading && <div className="h-[240px]" />}
          {/* "Unavailable" is reserved for a series the server did NOT serve —
              a served short series is insufficient history, not unavailability. */}
          {!q.isLoading && points.length === 0 && (
            <p className="text-sm text-ink-muted">
              {series
                ? 'No complete daily volume buckets to chart yet.'
                : 'The SDEX volume series is unavailable right now.'}
            </p>
          )}
          {!q.isLoading && points.length === 1 && (
            <p className="text-sm text-ink-muted">
              One complete day of volume so far —{' '}
              <span className="font-mono tabular-nums text-ink">
                {points[0].date}: ${formatCompact(points[0].value)}
              </span>
              . The chart appears once a second complete day lands.
            </p>
          )}
          {points.length >= 2 && (
            <VolumeLineChart
              data={points}
              height={240}
              positive
              ariaLabel={`Daily SDEX USD volume over the last ${points.length} days`}
              legend={{
                valueLabel: 'USD volume',
                formatValue: (n) => `$${formatCompact(n)}`,
              }}
            />
          )}
        </>
      )}
    </Panel>
  );
}

function formatKpi(k: BespokeKpi): string {
  const n = Number(k.value);
  if (!Number.isFinite(n)) return k.value;
  return k.unit === 'USD' ? `$${formatCompact(n)}` : formatCompact(n);
}
