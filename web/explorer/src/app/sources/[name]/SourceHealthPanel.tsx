'use client';

import { useSourceHealth } from '@/api/hooks';
import { formatCompact } from '@/lib/format';

/**
 * SourceHealthPanel — the live health pane on /sources/[name].
 *
 * Consumes `/v1/sources/{name}/health` (board #33): the venue's
 * registry metadata joined with trailing-24h liveness counters,
 * served from the API's 15s-refreshed ingestion snapshot. Client-
 * side + polling at that cadence so the pane tracks the venue in
 * near-real-time on a statically-exported page.
 *
 * `entries_24h` is the universal liveness signal (every decoded
 * event, so it's non-zero for oracles/FX/bridges too);
 * `trade_count_24h` / volume / markets are trades-table aggregates
 * that are legitimately 0 for non-trade sources.
 */
export function SourceHealthPanel({ source }: { source: string }) {
  const { data, isLoading, error } = useSourceHealth(source);

  return (
    <section className="border-line bg-surface rounded-lg border p-4">
      <header className="mb-3 flex items-baseline justify-between">
        <h2 className="text-ink-body text-sm font-semibold tracking-wider uppercase">
          Live health
        </h2>
        <span className="text-ink-faint text-xs">
          /v1/sources/{source}/health · refreshes every 15s
        </span>
      </header>

      {isLoading && (
        <p className="text-ink-muted text-sm">Loading live health…</p>
      )}
      {error != null && !isLoading && (
        <p className="text-ink-muted text-sm">
          Live health unavailable right now — the registry profile above is
          still authoritative for this venue&apos;s configuration.
        </p>
      )}
      {data && (
        <dl className="grid grid-cols-2 gap-3 text-sm sm:grid-cols-4">
          <HealthStat
            label="Events seen (24h)"
            value={data.entries_24h?.toLocaleString('en-US') ?? '—'}
            tone={data.entries_24h ? 'ok' : 'warn'}
            sub={
              data.entries_24h
                ? 'decoded events, all types'
                : 'no decoded events in 24h'
            }
          />
          <HealthStat
            label="Trades (24h)"
            value={data.trade_count_24h.toLocaleString('en-US')}
            sub="trades-table rows"
          />
          <HealthStat
            label="Volume (24h)"
            value={
              data.volume_24h_usd
                ? `$${formatCompact(data.volume_24h_usd)}`
                : '—'
            }
            sub="USD notional"
          />
          <HealthStat
            label="Markets (24h)"
            value={data.markets_count_24h.toLocaleString('en-US')}
            sub="distinct pairs traded"
          />
        </dl>
      )}
    </section>
  );
}

function HealthStat({
  label,
  value,
  sub,
  tone,
}: {
  label: string;
  value: string;
  sub?: string;
  tone?: 'ok' | 'warn';
}) {
  const valueClass =
    tone === 'ok' ? 'text-up' : tone === 'warn' ? 'text-warn-700' : '';
  return (
    <div>
      <dt className="text-ink-muted text-[10px] tracking-wider uppercase">
        {label}
      </dt>
      <dd className={`mt-1 font-mono text-sm tabular-nums ${valueClass}`}>
        {value}
      </dd>
      {sub && <div className="text-ink-faint mt-0.5 text-[11px]">{sub}</div>}
    </div>
  );
}
