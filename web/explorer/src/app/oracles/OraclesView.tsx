'use client';

import Link from 'next/link';
import { useMemo } from 'react';
import { useQuery } from '@tanstack/react-query';

import { Panel } from '@/components/reveal';
import { AssetLink } from '@/components/AssetLink';
import { apiGet, asExample } from '@/api/client';
import { isRawOracleAsset, rawOracleSymbol } from '@/lib/asset-label';
import { formatRelative , formatSubunitPrice } from '@/lib/format';
import { sourceToneClass } from '@/lib/pillTone';

// Wire shapes from the generated OpenAPI contract: /v1/sources rows via
// the shared hooks alias; /v1/oracle/streams rows are the spec's
// OracleReading schema.
import type { Source as SourceRow } from '@/api/hooks';
import type { components } from '@/api/types';

import { Container } from '@/components/ui';
type OracleStream = components['schemas']['OracleReading'];

// Oracle capture-totality: /v1/oracle/streams omits `raw:<symbol>` rows
// (symbols the oracle publishes that map to no canonical asset) unless the
// caller opts in. This page is the intended opt-in — totality is the point
// of the explorer — but unmapped rows are reference-only, so they render
// in their own "Unmapped feeds" section and never join the mapped list.
const STREAMS_PARAMS = { include_unmapped: 'true' } as const;

// The `mapped` flag is the contract; the `raw:` wire prefix is the same
// fact read off the asset id, kept as a belt-and-braces guard so a row
// can never land in the mapped table by a missing flag alone.
function isUnmappedStream(s: OracleStream): boolean {
  return s.mapped === false || isRawOracleAsset(s.asset);
}

export function OraclesView() {
  const sources = useQuery<SourceRow[]>({
    queryKey: ['/v1/sources', 'stats', 'oracle'],
    queryFn: async () => {
      const env = await apiGet<{ data: SourceRow[] }>('/v1/sources', { include: 'stats' });
      const arr = env.data ?? [];
      return arr.filter((s) => s.class === 'oracle').sort((a, b) => a.name.localeCompare(b.name));
    },
    refetchInterval: 60_000,
  });

  const streams = useQuery<OracleStream[]>({
    queryKey: ['/v1/oracle/streams', STREAMS_PARAMS],
    queryFn: async () => {
      const env = await apiGet<{ data: OracleStream[] }>('/v1/oracle/streams', STREAMS_PARAMS);
      return env.data ?? [];
    },
    refetchInterval: 60_000,
  });

  const oracles = sources.data ?? [];
  // Absent (fetch failed) ≠ empty (nothing registered / nothing observed).
  // Both tables below assert absence in their empty state; that claim is
  // only ours to make when the request actually answered.
  const registryAvailable = sources.data != null;
  const streamsAvailable = streams.data != null;
  // Memoise the `streams.data ?? []` defaulting so the perSourceCounts
  // useMemo dep array stays referentially stable across renders. Without
  // this, `streamRows` is a fresh `[]` literal on every render when
  // streams.data is undefined, making the downstream useMemo recompute
  // every tick. F-1258 (audit-2026-05-12).
  const allStreamRows = useMemo(() => streams.data ?? [], [streams.data]);
  // Mapped rows drive the "Price streams" table and the per-oracle
  // activity counts; unmapped rows are listed separately, below.
  const streamRows = useMemo(
    () => allStreamRows.filter((s) => !isUnmappedStream(s)),
    [allStreamRows],
  );
  const unmappedRows = useMemo(
    () => allStreamRows.filter(isUnmappedStream),
    [allStreamRows],
  );

  const perSourceCounts = useMemo(() => {
    const map: Record<string, { streams: number; latestTs: string }> = {};
    for (const s of streamRows) {
      const cur = map[s.source];
      if (!cur || s.ts > cur.latestTs) {
        map[s.source] = {
          streams: (cur?.streams ?? 0) + 1,
          latestTs: cur ? (s.ts > cur.latestTs ? s.ts : cur.latestTs) : s.ts,
        };
      } else {
        map[s.source].streams++;
      }
    }
    return map;
  }, [streamRows]);

  return (
    <Container className="space-y-6 py-8">
      <header className="space-y-2">
        <h1 className="text-3xl font-semibold tracking-tight">Oracles</h1>
        <p className="max-w-3xl text-sm text-ink-body">
          Every on-chain Stellar oracle we ingest and cross-reference.
          Oracles are reported alongside our independent VWAP but never
          included in it — mixing them would import their methodology
          and double-count whichever upstream markets they read.
        </p>
      </header>

      <Panel
        title="Connected oracles"
        hint="Per-oracle 24h activity"
        source={asExample('/v1/sources', { class: 'oracle', include: 'stats' })}
        bodyClassName="-mx-4"
      >
        <div className="overflow-x-auto">
          <table className="min-w-full divide-y divide-line text-sm">
            <thead>
              <tr className="text-left text-[10px] uppercase tracking-wider text-ink-muted">
                <Th>Oracle</Th>
                <Th align="right">Active streams</Th>
                <Th align="right">Last update</Th>
                <Th align="right">In VWAP?</Th>
              </tr>
            </thead>
            <tbody className="divide-y divide-line-subtle">
              {sources.isLoading && (
                <tr>
                  <td colSpan={4} className="px-4 py-6 text-center text-sm text-ink-muted">
                    Loading oracles…
                  </td>
                </tr>
              )}
              {!sources.isLoading && !registryAvailable && (
                <tr>
                  <td colSpan={4} className="px-4 py-6 text-center text-sm text-ink-muted">
                    Oracle registry unavailable right now — retry shortly.
                  </td>
                </tr>
              )}
              {!sources.isLoading && registryAvailable && oracles.length === 0 && (
                <tr>
                  <td colSpan={4} className="px-4 py-6 text-center text-sm text-ink-muted">
                    No oracles registered.
                  </td>
                </tr>
              )}
              {oracles.map((o) => {
                const perSrc = perSourceCounts[o.name];
                const tone = sourceToneClass(o.name);
                return (
                  <tr key={o.name} className="hover:bg-surface-muted">
                    <Td>
                      <Link
                        href={`/sources/${o.name}`}
                        className={`inline-block rounded-sm px-1.5 py-0.5 text-[11px] font-medium uppercase tracking-wider hover:underline ${tone}`}
                      >
                        {o.name}
                      </Link>
                    </Td>
                    <Td align="right">
                      <span className="font-mono tabular-nums text-ink-body">
                        {perSrc ? perSrc.streams : '—'}
                      </span>
                    </Td>
                    <Td align="right">
                      <span className="font-mono text-xs text-ink-muted">
                        {perSrc ? formatRelative(perSrc.latestTs) : '—'}
                      </span>
                    </Td>
                    <Td align="right">
                      <span
                        className={`inline-block rounded px-1.5 py-0.5 text-[10px] uppercase tracking-wider ${
                          o.class === 'oracle'
                            ? 'bg-line text-ink-body'
                            : 'bg-up-subtle text-up-strong'
                        }`}
                        title="Oracle observations are reported but never included in the canonical VWAP — that would import their methodology."
                      >
                        no (policy)
                      </span>
                    </Td>
                  </tr>
                );
              })}
            </tbody>
          </table>
        </div>
      </Panel>

      <Panel
        title={`Price streams${streamRows.length > 0 ? ` (${streamRows.length} active)` : ''}`}
        hint="Latest observation per (oracle, asset, quote) — 7d window"
        source={asExample('/v1/oracle/streams', STREAMS_PARAMS)}
        bodyClassName="-mx-4"
      >
        <div className="overflow-x-auto">
          <table className="min-w-full divide-y divide-line text-sm">
            <thead>
              <tr className="text-left text-[10px] uppercase tracking-wider text-ink-muted">
                <Th>Oracle</Th>
                <Th>Asset</Th>
                <Th>Quote</Th>
                <Th align="right">Latest price</Th>
                <Th align="right">Updated</Th>
              </tr>
            </thead>
            <tbody className="divide-y divide-line-subtle">
              {streams.isLoading && (
                <tr>
                  <td colSpan={5} className="px-4 py-6 text-center text-sm text-ink-muted">
                    Loading streams…
                  </td>
                </tr>
              )}
              {!streams.isLoading && !streamsAvailable && (
                <tr>
                  <td colSpan={5} className="px-4 py-6 text-center text-sm text-ink-muted">
                    Oracle observations unavailable right now — retry shortly.
                  </td>
                </tr>
              )}
              {!streams.isLoading && streamsAvailable && streamRows.length === 0 && (
                <tr>
                  <td colSpan={5} className="px-4 py-6 text-center text-sm text-ink-muted">
                    No oracle observations in the last 7 days.
                  </td>
                </tr>
              )}
              {streamRows.map((s, i) => {
                const tone = sourceToneClass(s.source);
                return (
                  <tr key={`${s.source}|${s.asset}|${s.quote}|${i}`} className="hover:bg-surface-muted">
                    <Td>
                      <Link
                        href={`/sources/${s.source}`}
                        className={`inline-block rounded-sm px-1.5 py-0.5 text-[10px] font-medium uppercase tracking-wider hover:underline ${tone}`}
                      >
                        {s.source}
                      </Link>
                    </Td>
                    <Td>
                      <AssetLink canonical={s.asset} />
                    </Td>
                    <Td>
                      <AssetLink canonical={s.quote} />
                    </Td>
                    <Td align="right">
                      <span className="font-mono tabular-nums text-ink-body">
                        {formatPrice(s.price)}
                      </span>
                    </Td>
                    <Td align="right">
                      <span className="font-mono text-xs text-ink-muted">
                        {formatRelative(s.ts)}
                      </span>
                    </Td>
                  </tr>
                );
              })}
            </tbody>
          </table>
        </div>
      </Panel>

      {streamsAvailable && (
        <Panel
          title={`Unmapped feeds${unmappedRows.length > 0 ? ` (${unmappedRows.length})` : ''}`}
          hint="Symbols published under no canonical asset — recorded verbatim, reference-only"
          source={asExample('/v1/oracle/streams', STREAMS_PARAMS)}
          bodyClassName="-mx-4"
        >
          <p className="px-4 pb-3 text-xs text-ink-muted">
            An oracle sometimes publishes a symbol we cannot map to a canonical
            asset. Capture totality means the observation is still recorded —
            under its raw on-wire symbol — but it is never compared, aggregated,
            or attributed to an asset page. These rows are listed here and kept
            out of the price-stream table above.
          </p>
          <div className="overflow-x-auto">
            <table className="min-w-full divide-y divide-line text-sm">
              <thead>
                <tr className="text-left text-[10px] uppercase tracking-wider text-ink-muted">
                  <Th>Oracle</Th>
                  <Th>Raw symbol</Th>
                  <Th>Quote (assumed)</Th>
                  <Th align="right">Latest price</Th>
                  <Th align="right">Updated</Th>
                </tr>
              </thead>
              <tbody className="divide-y divide-line-subtle">
                {unmappedRows.length === 0 && (
                  <tr>
                    <td colSpan={5} className="px-4 py-6 text-center text-sm text-ink-muted">
                      No unmapped feeds in the last 7 days — every published
                      symbol maps to a canonical asset.
                    </td>
                  </tr>
                )}
                {unmappedRows.map((s, i) => {
                  const tone = sourceToneClass(s.source);
                  return (
                    <tr
                      key={`${s.source}|${s.asset}|${s.quote}|${i}`}
                      className="hover:bg-surface-muted"
                    >
                      <Td>
                        <Link
                          href={`/sources/${s.source}`}
                          className={`inline-block rounded-sm px-1.5 py-0.5 text-[10px] font-medium uppercase tracking-wider hover:underline ${tone}`}
                        >
                          {s.source}
                        </Link>
                      </Td>
                      <Td>
                        <span
                          className="font-mono text-xs text-ink-body"
                          title={`${s.asset} — unmapped oracle symbol`}
                        >
                          {isRawOracleAsset(s.asset) ? rawOracleSymbol(s.asset) : s.asset}
                        </span>
                      </Td>
                      <Td>
                        {/* NOT a link, and labelled "assumed", because for an
                            unmapped row the quote is a DEFAULT rather than an
                            observation. The decoders fall back to fiat:USD for
                            any feed whose symbol carries no recognisable fiat
                            suffix — so a hypothetical wstETH/ETH or a bare
                            wBTC_FUNDAMENTAL would display "USD" beside a number
                            that is not dollars (wave-D SI-OC-01). mapped=false
                            already means the denomination is unknown by design;
                            rendering the default as though it were a fact is the
                            one place that contradiction reaches a reader. */}
                        <span
                          className="font-mono text-xs text-ink-muted"
                          title={`${s.quote} is the decoder's DEFAULT for an unmapped symbol, not an observed denomination — this row's true quote is unknown`}
                        >
                          {s.quote}
                        </span>
                      </Td>
                      <Td align="right">
                        <span className="font-mono tabular-nums text-ink-body">
                          {formatPrice(s.price)}
                        </span>
                      </Td>
                      <Td align="right">
                        <span className="font-mono text-xs text-ink-muted">
                          {formatRelative(s.ts)}
                        </span>
                      </Td>
                    </tr>
                  );
                })}
              </tbody>
            </table>
          </div>
        </Panel>
      )}

      <Panel
        title="SEP-40 compatibility"
        hint="Drop-in oracle interface"
        source={asExample('/v1/oracle/lastprice', { asset: 'native' })}
        bodyClassName="space-y-2 text-sm text-ink-body"
      >
        <p>
          We expose three SEP-40 endpoints —{' '}
          <code className="font-mono text-xs">/v1/oracle/lastprice</code>,{' '}
          <code className="font-mono text-xs">/v1/oracle/prices</code>,{' '}
          <code className="font-mono text-xs">/v1/oracle/x_last_price</code>{' '}
          — that match the SEP-40 contract trait on-chain consumers
          already integrate against. Routing your existing on-chain{' '}
          <code className="font-mono text-xs">lastprice()</code> calls
          through Stellar Index swaps in independent VWAP-backed prices
          without touching the calling contract.
        </p>
      </Panel>
    </Container>
  );
}

function Th({ children, align }: { children: React.ReactNode; align?: 'left' | 'right' }) {
  return (
    <th
      scope="col"
      className={`px-4 py-2 ${align === 'right' ? 'text-right' : 'text-left'}`}
    >
      {children}
    </th>
  );
}

function Td({ children, align }: { children: React.ReactNode; align?: 'left' | 'right' }) {
  return (
    <td className={`px-4 py-2 ${align === 'right' ? 'text-right' : 'text-left'}`}>{children}</td>
  );
}

function formatPrice(p: string): string {
  const n = Number(p);
  if (!Number.isFinite(n)) return p;
  if (n === 0) return '0';
  if (n >= 1) return n.toFixed(4);
  if (n >= 0.01) return n.toFixed(6);
  return formatSubunitPrice(n);
}
