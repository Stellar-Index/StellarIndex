'use client';

import { useMemo, useState } from 'react';
import Link from 'next/link';

import { Panel } from '@/components/reveal';
import { asExample } from '@/api/client';
import {
  useSources,
  useCursors,
  isOnChainSource,
  type Source,
} from '@/api/hooks';
import { formatDurationShort } from '@/lib/format';
import { Badge, Input, TBody, TR, Table, Td, Th, THead } from '@/components/ui';
import { CountBar, LagDot } from './SourceListMarks';

/**
 * Live sources directory backed by `/v1/sources`.
 *
 * Groups by `class` (exchange / aggregator / oracle / authority_sanity)
 * — only Class=exchange sources contribute to VWAP by default; the
 * rest are reported alongside but excluded so we don't double-count
 * upstream markets or import their methodology. Per-source flags
 * (`include_in_vwap`, `paid`, `backfill_available`, `backfill_safe`)
 * surface as badges in the last column.
 */
const SOURCE_CSV_COLUMNS = [
  'name',
  'subclass',
  'default_weight',
  'trade_count_24h',
] as const;

export function SourcesTable() {
  // includeStats=true joins per-source 24h trade counts so the
  // table can show the most-active venues at the top of each
  // class group. The opt-in matches the public docs so any caller
  // using /v1/sources directly sees the same shape.
  const { data, isLoading, isError, error } = useSources(undefined, true);
  const cursors = useCursors();
  const [filter, setFilter] = useState('');

  // Stellar-only directory: keep on-chain venues (DEX, on-chain oracles,
  // lending, routers, bridges); drop off-chain reference feeds (CEX /
  // FX / aggregators / Chainlink) — those are the pricing layer, not
  // Stellar network activity, and live on /exchanges + /aggregators.
  const stellar = useMemo(() => (data ?? []).filter(isOnChainSource), [data]);

  const filteredData = useMemo(() => {
    const q = filter.trim().toLowerCase();
    if (!q) return stellar;
    return stellar.filter((s) => {
      const hay = `${s.name} ${s.class} ${s.subclass ?? ''}`.toLowerCase();
      return hay.includes(q);
    });
  }, [stellar, filter]);

  const grouped = useMemo(() => groupByClass(filteredData), [filteredData]);

  // Aggregate the cursors slice by VENUE — one venue can have many
  // cursors (live + per-range backfills). The cursor's `source` field
  // is the cursor TYPE (backfill / projector / ledgerstream), NOT the
  // venue; the venue lives in `sub_source` (projector rows:
  // "soroswap"; backfill rows: "<range>:sdex"). Keying by `source`
  // would mean every venue's "Last
  // ingest" rendered "—". Surface the most-recent row's tip per venue.
  //
  // ONE-SHOT CURSORS ARE EXCLUDED. Picking the highest
  // last_ledger across all cursor types made SDEX — the busiest venue we
  // index, ingesting ~24k trades per 20 minutes — render "19d ago" in red
  // as if it were dead, because its highest-ledger row was a COMPLETED
  // backfill cursor last touched 19 days earlier. Live SDEX ingest rides
  // `ledgerstream` and has no per-venue cursor of its own, so the stale
  // historical job won.
  //
  // /v1/diagnostics/cursors is dominated by finished one-shot jobs (1,483
  // rows, of which exactly one is live; 1,202 projected-rebuild, 194
  // backfill). Those describe historical work,
  // never live freshness, so they must not feed a freshness column.
  const latestBySource = useMemo(() => {
    const m = new Map<
      string,
      { last_ledger: number; lag_seconds: number; last_updated: string }
    >();
    for (const c of cursors.data ?? []) {
      if (isOneShotCursor(c)) continue;
      const venue = cursorVenue(c);
      if (!venue) continue;
      const prev = m.get(venue);
      if (!prev || prev.last_ledger < c.last_ledger) {
        m.set(venue, {
          last_ledger: c.last_ledger,
          lag_seconds: c.lag_seconds,
          last_updated: c.last_updated,
        });
      }
    }
    return m;
  }, [cursors.data]);

  if (isError) {
    return (
      <Panel
        headingLevel={2}
        title="Sources"
        source={asExample('/v1/sources')}
        bodyClassName="text-sm text-bad-700"
      >
        Failed to load sources:{' '}
        {error instanceof Error ? error.message : 'unknown error'}
      </Panel>
    );
  }
  if (isLoading || !data) {
    return (
      <Panel
        headingLevel={2}
        title="Sources"
        source={asExample('/v1/sources')}
        bodyClassName="text-sm text-ink-muted"
      >
        Loading…
      </Panel>
    );
  }
  if (stellar.length === 0) {
    return (
      <Panel
        headingLevel={2}
        title="Sources"
        source={asExample('/v1/sources')}
        bodyClassName="text-sm text-ink-muted"
      >
        No Stellar on-chain sources registered.
      </Panel>
    );
  }

  return (
    <div className="space-y-4">
      <div className="flex flex-wrap items-center gap-3">
        <Input
          type="search"
          aria-label="Filter sources by name, class, or subclass"
          placeholder="Filter by name, class or subclass…"
          value={filter}
          onChange={(e) => setFilter(e.target.value)}
          className="w-72"
        />
        <span className="text-ink-muted tnum text-xs">
          {filteredData.length} of {stellar.length} sources
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
      {filter && grouped.length === 0 && (
        <Panel
          headingLevel={2}
          title="Sources"
          source={asExample('/v1/sources')}
          bodyClassName="text-sm text-ink-muted"
        >
          No sources match &quot;{filter}&quot;.
        </Panel>
      )}
      {grouped.map(({ klass, rows }) => {
        const maxTrades = rows.reduce(
          (m, r) => Math.max(m, r.trade_count_24h ?? 0),
          0,
        );
        return (
          <Panel
            headingLevel={2}
            key={klass}
            title={titleCase(klass)}
            hint={`${rows.length} ${rows.length === 1 ? 'source' : 'sources'} · ${classHint(klass)}`}
            source={asExample('/v1/sources', { class: klass })}
            download={{
              name: `sources-${klass}`,
              columns: SOURCE_CSV_COLUMNS,
              rows: rows.map((s) => ({
                name: s.name,
                subclass: s.subclass,
                default_weight: s.default_weight,
                trade_count_24h: s.trade_count_24h,
              })),
            }}
            bodyClassName="-mx-4 -mb-4"
          >
            <div className="overflow-x-auto">
              <Table>
                <THead>
                  <tr>
                    <Th>Source</Th>
                    <Th className="hidden sm:table-cell">Subclass</Th>
                    <Th align="right">Weight</Th>
                    <Th align="right">24h trades</Th>
                    <Th align="right">Last ingest</Th>
                    <Th align="right">Flags</Th>
                  </tr>
                </THead>
                <TBody>
                  {rows.map((s) => (
                    <TR key={s.name}>
                      <Td>
                        <Link
                          href={`/sources/${encodeURIComponent(s.name)}`}
                          className="text-ink hover:text-brand-600 font-mono text-xs"
                        >
                          {s.name}
                        </Link>
                      </Td>
                      <Td className="text-ink-muted hidden sm:table-cell">
                        {s.subclass ?? '—'}
                      </Td>
                      <Td align="right" className="font-mono">
                        {s.default_weight}
                      </Td>
                      <Td align="right" className="font-mono">
                        <div className="flex items-center justify-end gap-2">
                          <CountBar
                            value={s.trade_count_24h ?? 0}
                            max={maxTrades}
                          />
                          {typeof s.trade_count_24h === 'number' &&
                          s.trade_count_24h > 0 ? (
                            <span>
                              {s.trade_count_24h.toLocaleString('en-US')}
                            </span>
                          ) : (
                            <span className="text-ink-faint">—</span>
                          )}
                        </div>
                      </Td>
                      <Td align="right">
                        <CursorAgo cursor={latestBySource.get(s.name)} />
                      </Td>
                      <Td align="right">
                        <div className="flex flex-wrap justify-end gap-1">
                          {s.include_in_vwap && (
                            <Badge tone="brand">in VWAP</Badge>
                          )}
                          {s.paid && <Badge>paid</Badge>}
                          {s.backfill_available && !s.backfill_safe && (
                            <Badge tone="warn">backfill unaudited</Badge>
                          )}
                          {s.backfill_safe && (
                            <Badge tone="ok">backfill safe</Badge>
                          )}
                          {!s.backfill_available && <Badge>live-only</Badge>}
                        </div>
                      </Td>
                    </TR>
                  ))}
                </TBody>
              </Table>
            </div>
          </Panel>
        );
      })}
    </div>
  );
}

function CursorAgo({
  cursor,
}: {
  cursor:
    | { last_ledger: number; lag_seconds: number; last_updated: string }
    | undefined;
}) {
  if (!cursor) {
    return <span className="text-ink-faint">—</span>;
  }
  // Tone bucket: <60s green, <10min amber, else red. Matches the
  // home System health panel's threshold so the two views agree
  // on what "live" means.
  const tone =
    cursor.lag_seconds < 60
      ? 'text-ok-700'
      : cursor.lag_seconds < 600
        ? 'text-warn-700'
        : 'text-bad-700';
  return (
    <div className="text-right">
      <div className={`font-mono text-xs ${tone}`}>
        <LagDot lagSeconds={cursor.lag_seconds} />
        {formatDurationShort(cursor.lag_seconds)} ago
      </div>
      <div className="text-ink-faint font-mono text-[11px]">
        #{cursor.last_ledger.toLocaleString('en-US')}
      </div>
    </div>
  );
}

const ONE_SHOT_CURSOR_TYPES = new Set([
  'backfill',
  'backfill-router',
  'census-backfill',
  'projected-rebuild',
  'gap-detector-scan',
]);

function isOneShotCursor(c: { source?: string }): boolean {
  return ONE_SHOT_CURSOR_TYPES.has((c.source ?? '').trim());
}

function cursorVenue(c: { sub_source?: string }): string {
  const ss = (c.sub_source ?? '').trim();
  if (!ss) return '';
  const colon = ss.lastIndexOf(':');
  return colon >= 0 ? ss.slice(colon + 1) : ss;
}

function groupByClass(
  rows: Source[],
): { klass: Source['class']; rows: Source[] }[] {
  // Includes the on-chain non-exchange classes (lending / router /
  // bridge) — without them, blend / cctp / rozo / defindex /
  // soroswap-router fell into the map but were never emitted. The
  // off-chain classes (aggregator / authority_sanity) stay in the
  // order for resilience but are filtered out upstream now.
  const order: Source['class'][] = [
    'exchange',
    'oracle',
    'lending',
    'router',
    'bridge',
    'aggregator',
    'authority_sanity',
  ];
  const map = new Map<Source['class'], Source[]>();
  for (const r of rows) {
    const arr = map.get(r.class) ?? [];
    arr.push(r);
    map.set(r.class, arr);
  }
  const out: { klass: Source['class']; rows: Source[] }[] = [];
  for (const k of order) {
    const rs = map.get(k);
    if (rs && rs.length > 0) {
      // 24h trade count desc (most-active venues first), with
      // alpha-by-name as the tiebreaker for venues that have no
      // recent activity. Falls back to alpha-only when the caller
      // didn't request stats.
      rs.sort((a, b) => {
        const ta = a.trade_count_24h ?? 0;
        const tb = b.trade_count_24h ?? 0;
        if (ta !== tb) return tb - ta;
        return a.name.localeCompare(b.name);
      });
      out.push({ klass: k, rows: rs });
    }
  }
  return out;
}

function titleCase(s: string): string {
  return s.replace(/_/g, ' ').replace(/\b\w/g, (c) => c.toUpperCase());
}

function classHint(k: Source['class']): string {
  switch (k) {
    case 'exchange':
      return 'On-chain DEX venues — contribute to VWAP by default';
    case 'oracle':
      return 'On-chain price feeds (Reflector / Band / Redstone) — reported alongside, excluded from VWAP';
    case 'lending':
      return 'On-chain lending — auction stress-prices reported alongside, excluded from VWAP';
    case 'router':
      return 'On-chain DEX routers + aggregator vaults — per-tx attribution, excluded from VWAP';
    case 'bridge':
      return 'Cross-chain bridges — flow coverage (no prices), excluded from VWAP';
    case 'aggregator':
      return 'Reported alongside; excluded from VWAP to avoid double-counting upstream markets';
    case 'authority_sanity':
      return 'Authority sanity check — divergence reference, never priced into VWAP';
  }
}
