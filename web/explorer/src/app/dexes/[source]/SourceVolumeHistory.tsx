'use client';

import { useQuery } from '@tanstack/react-query';

import { Panel } from '@/components/reveal';
import { apiGet, asExample } from '@/api/client';
import { TimeSeriesChart } from '@/app/protocols/[name]/TimeSeriesChart';
import type { Bespoke, BespokeSeries } from '@/app/protocols/[name]/BespokeSection';

/**
 * isUsdVolumeSeries — matches the DEX bespoke block's standalone USD-volume
 * series. The backend's wire name is "USD volume"
 * (internal/storage/timescale/bespoke_dex.go dexSeriesVolume); we match
 * case-insensitively by substring so a cosmetic label edit upstream (e.g.
 * back to the pre-2026-07 "Daily USD volume") doesn't silently blank this
 * panel again — that exact mismatch shipped and nulled the chart on all
 * five /dexes pages. Grouped per-entity series ("Top pairs · XLM/USDC")
 * are excluded by the " · " separator so a pair name containing "USD"
 * can never match.
 */
export function isUsdVolumeSeries(name: string): boolean {
  return !name.includes(' · ') && name.toLowerCase().includes('usd volume');
}

/**
 * useDexBespoke — the shared fetch of /v1/protocols/{source}'s bespoke
 * block for the /dexes/[source] page. One query key for every consumer on
 * the page (this panel + DexAnalyticsSection) so the endpoint is hit once.
 */
export function useDexBespoke(source: string) {
  return useQuery<Bespoke | null>({
    queryKey: ['/v1/protocols/{name}', source, 'bespoke'],
    queryFn: async () => {
      const env = await apiGet<{ data?: { bespoke?: Bespoke } }>(
        `/v1/protocols/${encodeURIComponent(source)}`,
      );
      return env.data?.bespoke ?? null;
    },
    staleTime: 300_000,
    retry: false,
  });
}

/**
 * SourceVolumeHistory — the 90-day daily USD-volume chart for one DEX,
 * sourced from the protocol analytics endpoint's bespoke block
 * (`/v1/protocols/{name}` → the "USD volume" series, which reads the
 * dex_volume_by_pair_1d continuous aggregate over the server's default
 * 90d window). No new API: this is the existing aggregate the
 * /protocols page already charts, reused here so the venue page shows
 * history beyond the 24h/7d activity strip. Renders nothing when the
 * protocol has no volume series (the strip above still serves) —
 * absence, not zeros.
 */
export function SourceVolumeHistory({ source }: { source: string }) {
  const q = useDexBespoke(source);

  const series: BespokeSeries | null =
    q.data?.series?.find((s) => isUsdVolumeSeries(s.name)) ?? null;
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
