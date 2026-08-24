'use client';

import { useMemo, useState } from 'react';

import { Panel } from '@/components/reveal';
import { asExample } from '@/api/client';
import { useCursors, type Cursor } from '@/api/hooks';
import { formatRelative, formatDurationShort } from '@/lib/format';

/**
 * Live ingest-cursor table backed by `/v1/diagnostics/cursors`.
 *
 * Refetches every 15s so backfills visibly tick. Rows are grouped by
 * `source`; within a group they are ordered by `sub_source`. The
 * `lag_seconds` column gets a coloured pill — green when the cursor
 * advanced in the last 60s, amber up to 10 minutes, red beyond.
 */
// STALE_LAG_THRESHOLD_S — cursors that haven't advanced in this
// long are hidden by default. Backfill jobs that ran weeks ago
// leave their progress markers in the table forever; on r1 today
// roughly 30 of 50 rows are completed-and-forgotten, drowning out
// the live ingest cursor every operator opens this page to find.
// Threshold matches the LagPill red tier (>10 min) but with a
// generous floor so a slow-but-progressing backfill (an hour or
// two between updates) doesn't disappear.
const STALE_LAG_THRESHOLD_S = 3600;

export function CursorsTable() {
  const { data, isLoading, isError, error } = useCursors();
  const [filter, setFilter] = useState('');
  const [hideStale, setHideStale] = useState(true);

  const filtered = useMemo(() => {
    const q = filter.trim().toLowerCase();
    let rows = data ?? [];
    if (hideStale) {
      rows = rows.filter((c) => c.lag_seconds <= STALE_LAG_THRESHOLD_S);
    }
    if (q) {
      rows = rows.filter((c) => {
        const hay = `${c.source} ${c.sub_source ?? ''}`.toLowerCase();
        return hay.includes(q);
      });
    }
    return rows;
  }, [data, filter, hideStale]);

  const grouped = useMemo(() => groupBySource(filtered), [filtered]);

  if (isError) {
    return (
      <Panel
        title="Ingest cursors"
        source={asExample('/v1/diagnostics/cursors')}
        bodyClassName="text-sm text-down-strong"
      >
        Failed to load cursors:{' '}
        {error instanceof Error ? error.message : 'unknown error'}
      </Panel>
    );
  }
  if (isLoading || !data) {
    return (
      <Panel
        title="Ingest cursors"
        source={asExample('/v1/diagnostics/cursors')}
        bodyClassName="text-sm text-ink-muted"
      >
        Loading…
      </Panel>
    );
  }
  if (data.length === 0) {
    return (
      <Panel
        title="Ingest cursors"
        source={asExample('/v1/diagnostics/cursors')}
        bodyClassName="text-sm text-ink-muted"
      >
        No cursors recorded yet.
      </Panel>
    );
  }

  return (
    <Panel
      title="Ingest cursors"
      hint="Per-source progress markers — refreshed every 15s"
      source={asExample('/v1/diagnostics/cursors')}
      bodyClassName="-mx-4"
    >
      <div className="px-4 pt-1 pb-3">
        <div className="flex flex-wrap items-center gap-3 text-xs">
          <input
            type="search"
            aria-label="Filter cursors by source or sub-source"
            placeholder="Filter sources or sub-sources…"
            value={filter}
            onChange={(e) => setFilter(e.target.value)}
            className="border-line bg-surface placeholder:text-ink-faint focus:border-brand-500 focus:ring-brand-500 w-64 rounded-md border px-2.5 py-1 font-mono text-[11px] focus:ring-1 focus:outline-hidden"
          />
          <label className="text-ink-body inline-flex items-center gap-1.5 font-mono text-[11px] select-none">
            <input
              type="checkbox"
              checked={hideStale}
              onChange={(e) => setHideStale(e.target.checked)}
              className="border-line-strong text-brand-600 focus:ring-brand-500 h-3.5 w-3.5 rounded-sm"
            />
            Hide stale (&gt;1h)
          </label>
          <span className="text-ink-muted font-mono text-[11px]">
            {filtered.length} of {(data ?? []).length} rows
            {filter && (
              <button
                type="button"
                onClick={() => setFilter('')}
                className="text-brand-600 ml-2 hover:underline"
              >
                clear
              </button>
            )}
          </span>
        </div>
      </div>
      <div className="overflow-x-auto">
        <table className="divide-line min-w-full divide-y text-sm">
          <thead>
            <tr className="text-ink-muted text-left text-[11px] tracking-wider uppercase">
              <Th>Source</Th>
              <Th>Sub-source</Th>
              <Th align="right">Last ledger</Th>
              <Th align="right">Updated</Th>
              <Th align="right">Lag</Th>
            </tr>
          </thead>
          <tbody className="divide-line-subtle divide-y">
            {grouped.map(({ source, rows }) =>
              rows.map((c, i) => (
                <tr
                  key={`${c.source}|${c.sub_source ?? ''}`}
                  className="hover:bg-surface-muted"
                >
                  <Td>
                    {i === 0 ? (
                      <span className="font-medium">{source}</span>
                    ) : (
                      <span className="text-ink-faint">↳</span>
                    )}
                  </Td>
                  <Td>
                    <span className="text-ink-muted font-mono text-xs">
                      {c.sub_source || '—'}
                    </span>
                  </Td>
                  <Td align="right">
                    <span className="font-mono text-xs tabular-nums">
                      #{c.last_ledger.toLocaleString('en-US')}
                    </span>
                  </Td>
                  <Td align="right">
                    <span className="text-ink-muted font-mono text-xs tabular-nums">
                      {formatRelative(c.last_updated)}
                    </span>
                  </Td>
                  <Td align="right">
                    <LagPill seconds={c.lag_seconds} />
                  </Td>
                </tr>
              )),
            )}
          </tbody>
        </table>
      </div>
    </Panel>
  );
}

function LagPill({ seconds }: { seconds: number }) {
  const tone =
    seconds <= 60
      ? 'bg-up-soft text-up-strong'
      : seconds <= 600
        ? 'bg-warn-50 text-warn-700'
        : 'bg-down-soft text-down-strong';
  return (
    <span
      className={`inline-block rounded-sm px-1.5 py-0.5 font-mono text-[11px] tabular-nums ${tone}`}
    >
      {formatDurationShort(seconds)}
    </span>
  );
}

function groupBySource(rows: Cursor[]): { source: string; rows: Cursor[] }[] {
  const map = new Map<string, Cursor[]>();
  for (const r of rows) {
    const arr = map.get(r.source) ?? [];
    arr.push(r);
    map.set(r.source, arr);
  }
  const out: { source: string; rows: Cursor[] }[] = [];
  for (const [source, rs] of map) {
    rs.sort((a, b) => (a.sub_source ?? '').localeCompare(b.sub_source ?? ''));
    out.push({ source, rows: rs });
  }
  out.sort((a, b) => a.source.localeCompare(b.source));
  return out;
}

function Th({
  children,
  align,
}: {
  children: React.ReactNode;
  align?: 'left' | 'right';
}) {
  return (
    <th
      className={`px-4 py-2 ${align === 'right' ? 'text-right' : 'text-left'}`}
      scope="col"
    >
      {children}
    </th>
  );
}

function Td({
  children,
  align,
}: {
  children: React.ReactNode;
  align?: 'left' | 'right';
}) {
  return (
    <td
      className={`px-4 py-3 ${align === 'right' ? 'text-right' : 'text-left'}`}
    >
      {children}
    </td>
  );
}
