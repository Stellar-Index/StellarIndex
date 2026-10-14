'use client';

import { useSourceHealth } from '@/api/hooks';
import { asExample } from '@/api/client';
import { Panel } from '@/components/reveal';
import { Skeleton, Stat, StatCell, StatGrid } from '@/components/ui';
import { formatCompactUnits } from '@/lib/format';

/**
 * SourceHealthPanel — the live health pane on /sources/[name], from
 * `/v1/sources/{name}/health`: registry metadata joined with trailing-24h
 * liveness counters out of the API's 15s ingestion snapshot.
 *
 * `entries_24h` is the universal liveness signal (every decoded event, so
 * non-zero for oracles/FX/bridges too); trades, volume and markets are
 * trades-table aggregates that are legitimately 0 for non-trade sources.
 */
export function SourceHealthPanel({ source }: { source: string }) {
  const { data, isLoading, error } = useSourceHealth(source);

  return (
    <Panel
      headingLevel={2}
      title="Live health"
      hint="Trailing 24 hours · refreshes every 15s"
      source={asExample(`/v1/sources/${encodeURIComponent(source)}/health`)}
    >
      {isLoading && <Skeleton className="h-[5.5rem]" />}
      {error != null && !isLoading && (
        <p className="text-ink-muted text-sm">
          Live health unavailable right now — the registry profile above is
          still authoritative for this venue&apos;s configuration.
        </p>
      )}
      {data && (
        <StatGrid cols={4}>
          <StatCell>
            <Stat
              label="Events seen"
              value={
                <span
                  className={data.entries_24h ? 'text-ok-700' : 'text-warn-700'}
                >
                  {data.entries_24h?.toLocaleString('en-US') ?? '—'}
                </span>
              }
              sub={
                data.entries_24h
                  ? 'decoded events, all types'
                  : 'no decoded events in 24h'
              }
            />
          </StatCell>
          <StatCell>
            <Stat
              label="Trades"
              value={data.trade_count_24h.toLocaleString('en-US')}
              sub="trades-table rows"
            />
          </StatCell>
          <StatCell>
            <Stat
              label="Volume"
              value={
                data.volume_24h_usd
                  ? `$${formatCompactUnits(data.volume_24h_usd)}`
                  : '—'
              }
              sub="USD notional"
            />
          </StatCell>
          <StatCell>
            <Stat
              label="Markets"
              value={data.markets_count_24h.toLocaleString('en-US')}
              sub="distinct pairs traded"
            />
          </StatCell>
        </StatGrid>
      )}
    </Panel>
  );
}
