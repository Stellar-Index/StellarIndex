'use client';

import dynamic from 'next/dynamic';
import { useQuery } from '@tanstack/react-query';

import { Panel } from '@/components/reveal';
import { apiGet, asExample } from '@/api/client';
import { formatCompact } from '@/lib/format';

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
  const points = (series?.points ?? [])
    .map((pt) => ({
      time: Math.floor(Date.parse(`${pt.date}T00:00:00Z`) / 1000),
      value: Number(pt.value),
    }))
    .filter((pt) => Number.isFinite(pt.time) && Number.isFinite(pt.value));

  const kpis = (q.data?.bespoke?.kpis ?? []).slice(0, 3);

  return (
    <Panel
      title="SDEX volume"
      hint="Daily USD volume of priced SDEX trades — unpriced trades count toward trade totals but not USD sums."
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
      {q.isLoading && <div className="h-[240px]" />}
      {!q.isLoading && points.length < 2 && (
        <p className="text-sm text-ink-muted">
          The SDEX volume series is unavailable right now.
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
    </Panel>
  );
}

function formatKpi(k: BespokeKpi): string {
  const n = Number(k.value);
  if (!Number.isFinite(n)) return k.value;
  return k.unit === 'USD' ? `$${formatCompact(n)}` : formatCompact(n);
}
