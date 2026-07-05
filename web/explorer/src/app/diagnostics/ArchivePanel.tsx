'use client';

import { useArchiveReport } from '@/api/hooks';

/**
 * ArchivePanel — the archive-completeness panel on /diagnostics.
 *
 * Renders `/v1/diagnostics/archive`: the latest report the ADR-0017
 * archive-completeness daemon writes after its daily check → fix →
 * re-check cycle over the cross-anchor history archive. A clean
 * report means every expected checkpoint file is present; the
 * `scanned_at` age is itself a signal (a stale report means the
 * daily timer stopped firing).
 *
 * The endpoint legitimately 404s (daemon hasn't run on this host
 * yet) or 503s (deployment didn't configure a report path) — both
 * render the honest absence state, not an error wall.
 */
export function ArchivePanel() {
  // dataUpdatedAt is TanStack's fetch timestamp — a purity-safe "now"
  // proxy for the staleness badge (which only fires at >48h, so
  // fetch-time precision is plenty).
  const { data, isLoading, error, dataUpdatedAt } = useArchiveReport();

  if (isLoading) {
    return (
      <section className="rounded-md border border-line bg-surface p-4 text-sm text-ink-muted">
        Loading archive report…
      </section>
    );
  }
  if (error || !data) {
    return (
      <section className="rounded-md border border-line bg-surface p-4 text-sm text-ink-muted">
        No archive-completeness report served on this deployment yet —
        the daemon (
        <code className="rounded-sm bg-surface-subtle px-1 font-mono text-[13px]">
          stellarindex-ops archive-completeness verify
        </code>
        ) runs on a daily timer and its latest report appears here via{' '}
        <code className="rounded-sm bg-surface-subtle px-1 font-mono text-[13px]">
          /v1/diagnostics/archive
        </code>
        .
      </section>
    );
  }

  const ca = data.cross_anchor;
  const clean = (ca?.missing_count ?? 0) === 0;
  const scannedAgeHours =
    dataUpdatedAt > 0
      ? (dataUpdatedAt - new Date(data.scanned_at).getTime()) / 3_600_000
      : 0;

  return (
    <section className="rounded-lg border border-line bg-surface p-4">
      <header className="mb-3 flex flex-wrap items-baseline justify-between gap-2">
        <div className="flex items-baseline gap-3">
          <h3 className="text-sm font-semibold uppercase tracking-wider text-ink-body">
            Cross-anchor archive
          </h3>
          <span
            className={`rounded-sm px-2 py-0.5 font-mono text-xs ${
              clean ? 'bg-up-subtle text-up' : 'bg-down-subtle text-down'
            }`}
          >
            {clean ? 'complete' : `${ca?.missing_count ?? '?'} missing`}
          </span>
          {scannedAgeHours > 48 && (
            <span className="rounded-sm bg-warn-50 px-2 py-0.5 text-[11px] uppercase tracking-wider text-warn-700">
              report {scannedAgeHours.toFixed(0)}h old
            </span>
          )}
        </div>
        <span className="text-xs text-ink-faint">
          ADR-0017 · /v1/diagnostics/archive
        </span>
      </header>

      <dl className="grid grid-cols-2 gap-3 text-sm sm:grid-cols-4">
        <ArchiveStat
          label="Ledger range"
          value={`#${data.range.from.toLocaleString()} – #${data.range.to.toLocaleString()}`}
        />
        <ArchiveStat
          label="Checkpoints expected"
          value={ca ? ca.expected.toLocaleString() : '—'}
        />
        <ArchiveStat
          label="Checkpoints found"
          value={ca ? ca.found.toLocaleString() : '—'}
        />
        <ArchiveStat
          label="Last scan"
          value={`${data.scanned_at.replace('T', ' ').slice(0, 16)} UTC`}
        />
      </dl>

      {ca && ca.missing_count > 0 && (
        <p className="mt-3 text-xs text-ink-muted">
          Missing checkpoints{ca.truncated ? ' (list truncated)' : ''}:{' '}
          <span className="font-mono">
            {(ca.missing ?? []).slice(0, 12).map((m) => `#${m}`).join(', ')}
            {(ca.missing?.length ?? 0) > 12 ? ', …' : ''}
          </span>
        </p>
      )}
    </section>
  );
}

function ArchiveStat({ label, value }: { label: string; value: string }) {
  return (
    <div>
      <dt className="text-[10px] uppercase tracking-wider text-ink-muted">
        {label}
      </dt>
      <dd className="mt-1 font-mono text-sm tabular-nums">{value}</dd>
    </div>
  );
}
