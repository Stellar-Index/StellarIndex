'use client';

import { useQuery } from '@tanstack/react-query';

import { Panel } from '@/components/reveal';
import { apiGet, asExample } from '@/api/client';
import { TimeSeriesChart } from '@/app/protocols/[name]/TimeSeriesChart';
import type { Bespoke } from '@/app/protocols/[name]/BespokeSection';

/**
 * SourceVolumeHistory — the 90-day daily USD-volume chart for one DEX,
 * sourced from the protocol analytics endpoint's bespoke block
 * (`/v1/protocols/{name}` → bespoke.series "Daily USD volume", which
 * reads the dex_volume_by_pair_1d continuous aggregate). No new API:
 * this is the existing aggregate the /protocols page already charts,
 * reused here so the venue page shows history beyond the 24h/7d
 * activity strip. Renders nothing when the protocol has no volume
 * series (the strip above still serves) — absence, not zeros.
 */
export function SourceVolumeHistory({ source }: { source: string }) {
  const q = useQuery({
    queryKey: ['/v1/protocols/{name}', 'volume-history', source],
    queryFn: async () => {
      const env = await apiGet<{ data?: { bespoke?: Bespoke } }>(
        `/v1/protocols/${encodeURIComponent(source)}`,
      );
      return (
        env.data?.bespoke?.series?.find((s) => s.name === 'Daily USD volume') ?? null
      );
    },
    staleTime: 300_000,
    retry: false,
  });

  const series = q.data;
  if (!series || series.points.length === 0) return null;

  return (
    <Panel
      title="USD volume — 90d"
      hint="Daily summed USD volume (priced trades) from the protocol analytics aggregate"
      source={asExample(`/v1/protocols/${source}`)}
    >
      <TimeSeriesChart
        points={series.points.map((p) => ({ date: p.date, value: Number(p.value) }))}
        label="daily USD volume"
        unit="USD"
      />
    </Panel>
  );
}
