'use client';

import { useSourceHealth } from '@/api/hooks';

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
    <section className="rounded-lg border border-line bg-surface p-4">
      <header className="mb-3 flex items-baseline justify-between">
        <h2 className="text-sm font-semibold uppercase tracking-wider text-ink-body">
          Live health
        </h2>
        <span className="text-xs text-ink-faint">
          /v1/sources/{source}/health · refreshes every 15s
        </span>
      </header>

      {isLoading && (
        <p className="text-sm text-ink-muted">Loading live health…</p>
      )}
      {error != null && !isLoading && (
        <p className="text-sm text-ink-muted">
          Live health unavailable right now — the registry profile above is
          still authoritative for this venue&apos;s configuration.
        </p>
      )}
      {data && (
        <dl className="grid grid-cols-2 gap-3 text-sm sm:grid-cols-4">
          <HealthStat
            label="Events seen (24h)"
            value={data.entries_24h.toLocaleString()}
            tone={data.entries_24h > 0 ? 'ok' : 'warn'}
            sub={
              data.entries_24h > 0
                ? 'decoded events, all types'
                : 'no decoded events in 24h'
            }
          />
          <HealthStat
            label="Trades (24h)"
            value={data.trade_count_24h.toLocaleString()}
            sub="trades-table rows"
          />
          <HealthStat
            label="Volume (24h)"
            value={
              data.volume_24h_usd
                ? `$${formatCompactUsd(data.volume_24h_usd)}`
                : '—'
            }
            sub="USD notional"
          />
          <HealthStat
            label="Markets (24h)"
            value={data.markets_count_24h.toLocaleString()}
            sub="distinct pairs traded"
          />
        </dl>
      )}
    </section>
  );
}

function formatCompactUsd(raw: string): string {
  const n = Number(raw);
  if (!Number.isFinite(n)) return raw;
  if (n >= 1e9) return `${(n / 1e9).toFixed(2)}B`;
  if (n >= 1e6) return `${(n / 1e6).toFixed(2)}M`;
  if (n >= 1e3) return `${(n / 1e3).toFixed(1)}K`;
  return n.toFixed(2);
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
      <dt className="text-[10px] uppercase tracking-wider text-ink-muted">
        {label}
      </dt>
      <dd className={`mt-1 font-mono text-sm tabular-nums ${valueClass}`}>
        {value}
      </dd>
      {sub && <div className="mt-0.5 text-[11px] text-ink-faint">{sub}</div>}
    </div>
  );
}
