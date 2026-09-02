'use client';

import { useQuery } from '@tanstack/react-query';

import { Panel } from '@/components/reveal';
import { apiGet, asExample } from '@/api/client';
import { formatCompact } from '@/lib/format';
import { SourceActivityChart } from './SourceActivityChart';
import { useProtocolTvls } from '../useProtocolTvls';

interface VolumeBucket {
  hour: string;
  volume_usd: string;
  trade_count?: number;
}

interface SourceStats {
  name: string;
  trade_count_24h?: number;
  volume_24h_usd?: string;
  markets_count_24h?: number;
  volume_history_24h?: VolumeBucket[];
  volume_history_7d?: VolumeBucket[];
}

/**
 * SourceStatsPanel — client-side rendering of the per-source 24h
 * activity strip on /dexes/[source]. Was server-side at build
 * time, but `/v1/sources?include=stats,sparkline` can take 10-25s
 * under cold-cache conditions and the Next build's per-page 60s
 * budget exhausted itself trying to render 5 source pages
 * concurrently. Moving the fetch to the client means the build
 * is free of API dependence; users see "Loading…" briefly on
 * first paint, then the real numbers fill in.
 */
export function SourceStatsPanel({
  source,
  unitsLabel = 'pools',
}: {
  source: string;
  unitsLabel?: string;
}) {
  const { data } = useQuery<SourceStats | null>({
    queryKey: ['/v1/sources', 'stats+sparkline7d', source],
    queryFn: async () => {
      // 7d (`sparkline7d`) is now cheap: it reads the source_volume_1h
      // continuous aggregate (migration 0068) rather than scanning raw
      // trades, so the chart's 24h/7d toggle is live.
      const env = await apiGet<{ data: SourceStats[] }>('/v1/sources', {
        include: 'stats,sparkline,sparkline7d',
      });
      return env.data?.find((r) => r.name === source) ?? null;
    },
    staleTime: 60_000,
    refetchInterval: 60_000,
  });

  const trades = data?.trade_count_24h ?? 0;
  const volume = data?.volume_24h_usd ? Number(data.volume_24h_usd) : 0;
  const markets = data?.markets_count_24h ?? 0;

  // TVL comes from the /v1/protocols snapshot (background-refreshed
  // server-side). Absent for sources without an absolute reserve
  // source — the stat is only shown when a snapshot exists, never a
  // fabricated zero. A snapshot with unpriced pools is a lower bound
  // ("≥" prefix; provenance in the hover title).
  const tvl = useProtocolTvls().data?.[source];
  const tvlValue = tvl ? Number(tvl.tvl_usd) : 0;
  // Only DEX protocols with an absolute reserve source have a
  // snapshot — the stat disappears entirely (not a dash) for CEX
  // venues and reserve-less DEXes, since this panel is reused on
  // /exchanges/{name} and /sources/{name} too.
  const showTvl = tvl != null && Number.isFinite(tvlValue) && tvlValue > 0;

  return (
    <Panel
      headingLevel={2}
      title="24h activity"
      hint={`Live from /v1/sources?include=stats,sparkline,sparkline7d (source=${source})`}
      source={asExample('/v1/sources', { include: 'stats,sparkline,sparkline7d' })}
    >
      <div className={`grid grid-cols-2 gap-3 ${showTvl ? 'sm:grid-cols-4' : 'sm:grid-cols-3'}`}>
        <Stat
          label="24h volume"
          value={volume > 0 ? `$${formatCompact(volume)}` : '—'}
        />
        <Stat
          label="24h trades"
          value={trades > 0 ? formatCompact(trades) : '—'}
        />
        <Stat
          label={`24h ${unitsLabel}`}
          value={markets > 0 ? markets.toLocaleString('en-US') : '—'}
        />
        {showTvl && tvl && (
          <Stat
            label="TVL"
            value={`${tvl.unpriced_pools > 0 ? '≥ ' : ''}$${formatCompact(tvlValue)}`}
            title={tvl.basis}
          />
        )}
      </div>
      {data?.volume_history_24h && data.volume_history_24h.length > 0 && (
        <div className="mt-4 border-t border-line pt-3">
          <div className="flex items-baseline justify-between text-[10px] uppercase tracking-wider text-ink-muted">
            <span>Trades / hour</span>
            <span className="text-ink-faint">USD volume / hour (bars)</span>
          </div>
          <div className="mt-2">
            <SourceActivityChart
              buckets24h={data.volume_history_24h}
              buckets7d={data.volume_history_7d}
              height={200}
            />
          </div>
        </div>
      )}
    </Panel>
  );
}

function Stat({ label, value, title }: { label: string; value: string; title?: string }) {
  return (
    <div title={title}>
      <div className="text-[10px] uppercase tracking-wider text-ink-muted">
        {label}
      </div>
      <div className="mt-1 text-2xl font-semibold tabular-nums">{value}</div>
    </div>
  );
}

