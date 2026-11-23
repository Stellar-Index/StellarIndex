'use client';

import { useSourceHealth } from '@/api/hooks';
import { asExample } from '@/api/client';
import { Panel } from '@/components/reveal';
import { Skeleton, Stat, StatCell, StatGrid } from '@/components/ui';
import { formatCompactUnits } from '@/lib/format';
import { VOLUME_FLOOR_HINT } from '@/lib/supplyFloor';

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
            <EventsStat entries={data.entries_24h} enabled={data.enabled} />
          </StatCell>
          <StatCell>
            <CounterStat
              label="Trades"
              n={data.trade_count_24h}
              sub="trades-table rows"
            />
          </StatCell>
          <StatCell>
            <Stat
              label="Volume"
              value={
                data.volume_24h_usd ? (
                  <span
                    title={
                      data.volume_lower_bound ? VOLUME_FLOOR_HINT : undefined
                    }
                  >
                    {`${data.volume_lower_bound ? '≥ ' : ''}$${formatCompactUnits(data.volume_24h_usd)}`}
                  </span>
                ) : (
                  '—'
                )
              }
              sub="USD notional"
            />
          </StatCell>
          <StatCell>
            <CounterStat
              label="Markets"
              n={data.markets_count_24h}
              sub="distinct pairs traded"
            />
          </StatCell>
        </StatGrid>
      )}
    </Panel>
  );
}

// The API soft-fails a timed-out counter read to 0 with no flag, so a 0 is
// "nothing counted OR unread" and never renders as a measured zero.
const ZERO_OR_UNREAD = 'none counted, or the read failed';
const NOT_REPORTED = 'not reported by this API';

function CounterStat({
  label,
  n,
  sub,
}: {
  label: string;
  n: number | null | undefined;
  sub: string;
}) {
  if (n == null) return <Stat label={label} value="—" sub={NOT_REPORTED} />;
  if (n === 0) return <Stat label={label} value="—" sub={ZERO_OR_UNREAD} />;
  return <Stat label={label} value={n.toLocaleString('en-US')} sub={sub} />;
}

function EventsStat({
  entries,
  enabled,
}: {
  entries: number | null | undefined;
  enabled: boolean | undefined;
}) {
  if (entries != null && entries > 0) {
    return (
      <Stat
        label="Events seen"
        value={
          <span className="text-ok-700">{entries.toLocaleString('en-US')}</span>
        }
        sub="decoded events, all types"
      />
    );
  }
  if (entries === 0 && enabled === false) {
    return (
      <Stat
        label="Events seen"
        value="Off"
        sub="not switched on for this deployment"
      />
    );
  }
  return (
    <Stat
      label="Events seen"
      value={<span className="text-warn-700">—</span>}
      sub={entries == null ? NOT_REPORTED : ZERO_OR_UNREAD}
    />
  );
}
