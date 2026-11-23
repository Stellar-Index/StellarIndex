'use client';

import { useSourceHealth } from '@/api/hooks';
import { asExample } from '@/api/client';
import { Panel } from '@/components/reveal';
import { Skeleton, Stat, StatCell, StatGrid } from '@/components/ui';
import { formatCompactUnits, formatRelative } from '@/lib/format';
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
  const health = useSourceHealth(source);
  const { isLoading, error } = health;
  const data = health.data?.data;
  const staleAsOf =
    health.data?.flags?.stale === true ? health.data.as_of : undefined;

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
      {data && health.data?.flags?.stale === true && (
        <p className="text-ink-muted mb-3 text-sm">
          <span className="text-ink font-medium">Last known, not current</span>
          {' · '}
          <span>
            the ingestion snapshot could not be refreshed
            {staleAsOf
              ? `; these figures are from ${formatRelative(staleAsOf)}`
              : ''}
          </span>
        </p>
      )}
      {data?.enabled === false && (
        <p className="text-ink-muted mb-3 text-sm">
          <span className="text-ink font-medium">Off</span>
          {' · '}
          <span>not switched on for this deployment</span>
        </p>
      )}
      {data && (
        <StatGrid cols={4}>
          <StatCell>
            <EventsStat
              entries={data.entries_24h}
              unmeasured={data.entries_24h_unmeasured === true}
            />
          </StatCell>
          <StatCell>
            <CounterStat
              label="Trades"
              n={data.trade_count_24h}
              unmeasured={data.trade_stats_unmeasured === true}
              sub="trades-table rows"
            />
          </StatCell>
          <StatCell>
            <Stat
              label="Volume"
              value={
                data.trade_stats_unmeasured ? (
                  <Unread why={TRADES_UNREAD} />
                ) : data.volume_24h_usd ? (
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
              unmeasured={data.trade_stats_unmeasured === true}
              sub="distinct pairs traded"
            />
          </StatCell>
        </StatGrid>
      )}
    </Panel>
  );
}

const NOT_REPORTED = 'not reported by this API';
const EVENTS_UNREAD = 'event counter could not be read';
const TRADES_UNREAD = 'trade statistics could not be read';

function Unread({ why }: { why: string }) {
  return (
    <span className="text-warn-700" title={why}>
      —
    </span>
  );
}

function CounterStat({
  label,
  n,
  unmeasured,
  sub,
}: {
  label: string;
  n: number | null | undefined;
  unmeasured: boolean;
  sub: string;
}) {
  if (unmeasured)
    return (
      <Stat
        label={label}
        value={<Unread why={TRADES_UNREAD} />}
        sub={TRADES_UNREAD}
      />
    );
  if (n == null) return <Stat label={label} value="—" sub={NOT_REPORTED} />;
  return <Stat label={label} value={n.toLocaleString('en-US')} sub={sub} />;
}

function EventsStat({
  entries,
  unmeasured,
}: {
  entries: number | null | undefined;
  unmeasured: boolean;
}) {
  if (unmeasured)
    return (
      <Stat
        label="Events seen"
        value={<Unread why={EVENTS_UNREAD} />}
        sub={EVENTS_UNREAD}
      />
    );
  if (entries == null)
    return (
      <Stat
        label="Events seen"
        value={<span className="text-warn-700">—</span>}
        sub={NOT_REPORTED}
      />
    );
  return (
    <Stat
      label="Events seen"
      value={
        <span className={entries > 0 ? 'text-ok-700' : undefined}>
          {entries.toLocaleString('en-US')}
        </span>
      }
      sub="decoded events, all types"
    />
  );
}
