'use client';

import { useQuery } from '@tanstack/react-query';

import { Panel } from '@/components/reveal';
import { Stat, StatCell, StatGrid } from '@/components/ui';
import { PANEL_LABEL_CLASS } from '@/components/ui/Page';
import { apiGet, asExample } from '@/api/client';
import {
  compareDecimalStrings,
  formatCompact,
  formatCompactUnits,
} from '@/lib/format';
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
 * SourceStatsPanel — the per-source 24h activity strip, shared by the
 * DEX, exchange and source detail pages. Client-side: the
 * `include=stats,sparkline` read can take 10-25s cold, which exhausted
 * the static export's per-page budget when it ran at build time.
 */
export function SourceStatsPanel({
  source,
  unitsLabel = 'pools',
}: {
  source: string;
  unitsLabel?: string;
}) {
  const { data, isError, isPending } = useQuery<SourceStats | null>({
    queryKey: ['/v1/sources', 'stats+sparkline7d', source],
    queryFn: async () => {
      // 7d (`sparkline7d`) reads the source_volume_1h continuous
      // aggregate rather than raw trades, so the 24h/7d toggle is live.
      const env = await apiGet<{ data: SourceStats[] }>('/v1/sources', {
        include: 'stats,sparkline,sparkline7d',
      });
      return env.data?.find((r) => r.name === source) ?? null;
    },
    staleTime: 60_000,
    refetchInterval: 60_000,
  });

  // A failed fetch with nothing cached must not read as a quiet source:
  // only a value the API returned renders as a number, including 0.
  const failed = isError && data === undefined;
  const placeholder = isPending ? '…' : '—';
  const trades = data?.trade_count_24h;
  const volume = data?.volume_24h_usd
    ? formatCompactUnits(data.volume_24h_usd)
    : '—';
  const markets = data?.markets_count_24h;

  // TVL comes from the /v1/protocols snapshot. Only DEX protocols with an
  // absolute reserve source have one, so the stat disappears entirely
  // (never a dash or a fabricated zero) for CEX venues and reserve-less
  // DEXes. A snapshot with unpriced pools is a lower bound.
  const tvl = useProtocolTvls().data?.byProtocol[source];
  const showTvl = tvl != null && compareDecimalStrings(tvl.tvl_usd, '0') === 1;
  const tvlLowerBound = showTvl && tvl.unpriced_pools > 0;

  return (
    <Panel
      headingLevel={2}
      title="24h activity"
      hint="Trailing 24 hours · refreshes every minute"
      source={asExample('/v1/sources', {
        include: 'stats,sparkline,sparkline7d',
      })}
    >
      <StatGrid cols={showTvl ? 4 : 3}>
        <StatCell>
          <Stat
            label="24h volume"
            value={volume !== '—' ? `$${volume}` : placeholder}
          />
        </StatCell>
        <StatCell>
          <Stat
            label="24h trades"
            value={trades != null ? formatCompact(trades) : placeholder}
          />
        </StatCell>
        <StatCell>
          <Stat
            label={`24h ${unitsLabel}`}
            value={
              markets != null ? markets.toLocaleString('en-US') : placeholder
            }
          />
        </StatCell>
        {showTvl && (
          <StatCell>
            <Stat
              label={<span title={tvl.basis}>TVL</span>}
              value={
                <>
                  {tvlLowerBound && (
                    <span className="text-ink-muted font-sans" aria-hidden>
                      ≥{' '}
                    </span>
                  )}
                  ${formatCompactUnits(tvl.tvl_usd)}
                </>
              }
              sub={
                tvlLowerBound
                  ? `Lower bound · ${tvl.unpriced_pools} unpriced pools`
                  : undefined
              }
            />
          </StatCell>
        )}
      </StatGrid>
      {failed && (
        <p role="alert" className="text-ink-muted mt-3 text-sm">
          24h activity unavailable right now.
        </p>
      )}
      {data?.volume_history_24h && data.volume_history_24h.length > 0 && (
        <div className="mt-4">
          <div
            className={`${PANEL_LABEL_CLASS} flex items-baseline justify-between`}
          >
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
