'use client';

import Link from 'next/link';
import { useState } from 'react';
import { useQuery } from '@tanstack/react-query';

import { Panel } from '@/components/reveal';
import {
  Badge,
  Button,
  CopyButton,
  Skeleton,
  TBody,
  TR,
  Table,
  Td,
  Th,
  THead,
  LinkRow,
} from '@/components/ui';
import { HBarList } from '@/components/charts/Bars';
import { apiGet, asExample, type Envelope } from '@/api/client';
import { decimalOrNull, formatRelative } from '@/lib/format';
import type { paths } from '@/api/types';

import { LENDING_POOL_META, lendingProtocol } from './poolMeta';

// One /v1/lending/pools row, derived from the generated OpenAPI
// contract (src/api/types.ts, `make web-generate-api`).
type LendingPool = NonNullable<
  paths['/lending/pools']['get']['responses'][200]['content']['application/json']['data']
>[number];

type PoolSummary = NonNullable<LendingPool['summary']>;

const fmtUsd = (n: number) =>
  n >= 1e9
    ? `$${(n / 1e9).toFixed(2)}B`
    : n >= 1e6
      ? `$${(n / 1e6).toFixed(2)}M`
      : `$${Math.round(n).toLocaleString('en-US')}`;
const pctFmt = (n: number) => `${(n * 100).toFixed(2)}%`;

function basisNote(s: PoolSummary) {
  const total = s.reserve_count ?? 0;
  const priced = s.priced_reserve_count ?? 0;
  return total > 0 && priced < total
    ? `Priced reserves only (${priced} of ${total})`
    : 'Priced reserves only';
}

function StatCells({
  tvl,
  tvlTitle,
  borrowed = tvl,
  borrowedTitle,
  util,
  supply,
  borrow,
  title,
}: {
  tvl: string;
  tvlTitle?: string;
  borrowed?: string;
  borrowedTitle?: string;
  util: string;
  supply: string;
  borrow: string;
  title?: string;
}) {
  return (
    <>
      <Td
        align="right"
        className="text-ink font-mono font-medium tabular-nums"
        title={tvlTitle}
      >
        {tvl}
      </Td>
      <Td
        align="right"
        className="text-ink font-mono font-medium tabular-nums"
        title={borrowedTitle}
      >
        {borrowed}
      </Td>
      {[util, supply, borrow].map((v, i) => (
        <Td
          key={i}
          align="right"
          className="hidden font-mono tabular-nums sm:table-cell"
          title={v !== '—' ? title : undefined}
        >
          {v}
        </Td>
      ))}
    </>
  );
}

/**
 * PoolStats — TVL, utilization and weighted APRs from the row's server
 * `summary`, which totals the pool's /reserves view with exact decimals
 * over priced reserves only (an unpriced reserve is excluded, never $0).
 * A partial TVL renders as a lower bound. Module-scope so React keeps one
 * component identity across parent re-renders.
 */
function PoolStats({ row }: { row: LendingPool }) {
  const s = row.summary;
  if (s) {
    // Display-only: compact USD rounds far above float error.
    const tvl = decimalOrNull(s.tvl_usd);
    const borrowed = decimalOrNull(s.borrowed_usd);
    const dash = (v: number | null | undefined, fmt: (n: number) => string) =>
      v != null && Number.isFinite(v) ? fmt(v) : '—';
    // "≥$0" says nothing: a zero lower bound renders as unknown.
    const usd = (n: number, lb: boolean | undefined) =>
      lb && n === 0 ? '—' : (lb ? '≥' : '') + fmtUsd(n);
    return (
      <StatCells
        tvl={dash(tvl, (n) => usd(n, s.lower_bound))}
        tvlTitle={
          s.lower_bound
            ? `Lower bound: ${basisNote(s).toLowerCase()}`
            : (s.tvl_usd ?? undefined)
        }
        borrowed={dash(borrowed, (n) => usd(n, s.borrowed_lower_bound))}
        borrowedTitle={
          s.borrowed_usd == null
            ? 'No USD value: no priced reserve has a borrow'
            : s.borrowed_lower_bound
              ? `Lower bound: ${basisNote(s).toLowerCase()}`
              : s.borrowed_usd
        }
        util={dash(s.utilization_pct, (n) => `${n.toFixed(1)}%`)}
        supply={dash(s.supply_apr, pctFmt)}
        borrow={dash(s.borrow_apr, pctFmt)}
        title={basisNote(s)}
      />
    );
  }
  if (row.summary_pending && row.pool)
    return <PendingPoolStats pool={row.pool} />;
  const v = row.summary_error ? 'unavailable' : '—';
  return <StatCells tvl={v} util={v} supply={v} borrow={v} />;
}

/**
 * PendingPoolStats — the list was served before this pool's reserves view
 * was built. Fetching /reserves joins that build; the list polls until the
 * summary lands.
 */
function PendingPoolStats({ pool }: { pool: string }) {
  const q = useQuery({
    queryKey: ['/v1/lending/pools/{pool}/reserves', pool, 'pools-table'],
    staleTime: 300_000,
    retry: false,
    queryFn: () =>
      apiGet<unknown>(
        `/v1/lending/pools/${encodeURIComponent(pool)}/reserves`,
        {},
      ),
  });
  const v = q.isError ? 'unavailable' : '…';
  return <StatCells tvl={v} util={v} supply={v} borrow={v} />;
}

// A pending row's build can outlive its /reserves call (a 503 past the
// handler budget, or a /reserves answer from the HTTP cache), so the list
// polls the server for the summary instead of trusting that call.
export const PENDING_POLL_MS = 3_000;
export const PENDING_POLL_MAX = 20;

/** useLendingPools — the shared /v1/lending/pools read, polling while any summary is pending. */
export function useLendingPools() {
  return useQuery<{ rows: LendingPool[]; truncated: boolean }>({
    queryKey: ['/v1/lending/pools'],
    refetchInterval: (query) =>
      query.state.data?.rows.some((r) => r.summary_pending) &&
      query.state.dataUpdateCount < PENDING_POLL_MAX
        ? PENDING_POLL_MS
        : false,
    queryFn: async () => {
      const env = await apiGet<Envelope<LendingPool[] | null>>(
        '/v1/lending/pools',
        {},
      );
      return { rows: env.data ?? [], truncated: env.flags?.truncated === true };
    },
  });
}

/** No state read and no auction: nothing to show but dashes. */
export function isInactivePool(p: LendingPool): boolean {
  const s = p.summary;
  return (
    !p.summary_pending &&
    !p.summary_error &&
    (s == null || (s.tvl_usd == null && s.borrowed_usd == null)) &&
    (p.auctions_total ?? 0) === 0
  );
}

/** TVL descending; unpriced pools after priced ones, by auctions then users. */
export function sortPools(rows: readonly LendingPool[]): LendingPool[] {
  const tvl = (p: LendingPool) => decimalOrNull(p.summary?.tvl_usd) ?? -1;
  return [...rows].sort(
    (a, b) =>
      tvl(b) - tvl(a) ||
      (b.auctions_total ?? 0) - (a.auctions_total ?? 0) ||
      (b.unique_users_30d ?? 0) - (a.unique_users_30d ?? 0),
  );
}

export function LendingPoolsTable() {
  const q = useLendingPools();
  const [showInactive, setShowInactive] = useState(false);

  const all = q.data?.rows ?? [];
  const rows = sortPools(all);
  const inactive = rows.filter(isInactivePool);
  const listed = showInactive ? rows : rows.filter((p) => !isInactivePool(p));
  // A failed BLNT read serves the Blend rows alone; the list is a lower bound.
  const truncatedNote = q.data?.truncated ? (
    <p role="status" className="text-warn-700 mb-2 text-xs">
      Partial list: some pools could not be read, so pools and auction counts
      below are a lower bound.
    </p>
  ) : null;

  // Liquidation-auction distribution across pools — same fetched rows,
  // no extra request. Renders only when at least one pool has auctions.
  const auctionRows = rows
    .filter((p) => (p.auctions_total ?? 0) > 0)
    .sort((x, y) => (y.auctions_total ?? 0) - (x.auctions_total ?? 0));

  // Secondary to TVL: rendered after the pools table.
  const auctionsPanel = auctionRows.length > 0 && (
    <Panel
      title="Liquidation auctions by pool — all time"
      hint="Auction events from the indexed lending event stream; the risk view of which pools actually liquidate."
      source={asExample('/v1/lending/pools', {})}
    >
      {truncatedNote}
      <HBarList
        ariaLabel={`All-time liquidation auctions per pool: ${auctionRows
          .map(
            (p) =>
              `${poolChartLabel(p.pool ?? '', p.protocol)} ${(p.auctions_total ?? 0).toLocaleString('en-US')}`,
          )
          .join(', ')}`}
        items={auctionRows.map((p) => ({
          label: poolChartLabel(p.pool ?? '', p.protocol),
          value: p.auctions_total ?? 0,
          display: (p.auctions_total ?? 0).toLocaleString('en-US'),
          annotation:
            (p.auctions_24h ?? 0) > 0
              ? `${(p.auctions_24h ?? 0).toLocaleString('en-US')} in 24h`
              : undefined,
          title: p.pool ?? undefined,
        }))}
      />
    </Panel>
  );

  return (
    <>
      <Panel
        title={`Pools${rows.length > 0 ? ` (${rows.length})` : ''}`}
        hint="TVL and utilization read live from pool storage; auctions and users from the indexed event stream"
        source={asExample('/v1/lending/pools', {})}
        bodyClassName="-mx-4 -mb-4"
      >
        {q.data?.truncated && (
          <p role="status" className="text-warn-700 mx-4 mb-2 text-xs">
            Partial list: some pools could not be read, so this list is a lower
            bound.
          </p>
        )}
        <div className="overflow-x-auto">
          <Table>
            <THead>
              <tr>
                <Th>Pool</Th>
                <Th>Protocol</Th>
                <Th align="right">TVL</Th>
                <Th align="right">Borrowed</Th>
                <Th align="right" className="hidden sm:table-cell">
                  Utilization
                </Th>
                <Th align="right" className="hidden sm:table-cell">
                  Supply APR
                </Th>
                <Th align="right" className="hidden sm:table-cell">
                  Borrow APR
                </Th>
                <Th align="right" className="hidden md:table-cell">
                  Auctions
                </Th>
                <Th align="right" className="hidden xl:table-cell">
                  Users (30d)
                </Th>
              </tr>
            </THead>
            <TBody>
              {q.isLoading && (
                <TR className="hover:bg-transparent">
                  <Td colSpan={COLS}>
                    <Skeleton className="h-4 w-64" />
                  </Td>
                </TR>
              )}
              {/* A failed fetch is an availability problem, not an empty
                chain state — the two must never share one message. */}
              {!q.isLoading && q.isError && (
                <TR className="hover:bg-transparent">
                  <Td colSpan={COLS} className="text-ink-muted py-4">
                    The pools list is unavailable right now.
                    <Button
                      variant="secondary"
                      size="sm"
                      className="ml-3"
                      onClick={() => void q.refetch()}
                    >
                      Retry
                    </Button>
                  </Td>
                </TR>
              )}
              {!q.isLoading && !q.isError && rows.length === 0 && (
                <TR className="hover:bg-transparent">
                  <Td colSpan={COLS} className="text-ink-muted py-4">
                    No lending pools are indexed yet: none has an auction or a
                    position.
                  </Td>
                </TR>
              )}
              {listed.map((p) => {
                const poolId = p.pool ?? '';
                const meta = LENDING_POOL_META[poolId];
                const protocol = lendingProtocol(meta?.protocol ?? p.protocol);
                return (
                  <LinkRow href={`/lending/${poolId}`} key={poolId}>
                    <Td>
                      {/* Curated label where we have one; we don't invent pool names. */}
                      <Link
                        href={`/lending/${poolId}`}
                        className="text-brand-600 text-sm whitespace-nowrap hover:underline"
                        title={poolId}
                      >
                        {poolDisplayName(poolId, p.protocol)}
                      </Link>
                      <div className="text-ink-muted flex items-center gap-1 font-mono text-xs whitespace-nowrap">
                        <span title={poolId}>
                          {poolId.slice(0, 4)}…{poolId.slice(-4)}
                        </span>
                        <CopyButton value={poolId} />
                        {meta?.deployedAt && (
                          <span className="hidden xl:inline" title="Deployed">
                            · {meta.deployedAt}
                          </span>
                        )}
                      </div>
                    </Td>
                    <Td>
                      {protocol ? (
                        <Badge>{protocol.name}</Badge>
                      ) : (
                        <span className="text-ink-faint">—</span>
                      )}
                    </Td>
                    <PoolStats row={p} />
                    <Td
                      align="right"
                      className="text-ink-muted hidden font-mono tabular-nums md:table-cell"
                    >
                      {(p.auctions_total ?? 0).toLocaleString('en-US')}
                      {(p.auctions_24h ?? 0) > 0 && (
                        <div className="text-xs">
                          {(p.auctions_24h ?? 0).toLocaleString('en-US')} in 24h
                        </div>
                      )}
                    </Td>
                    <Td
                      align="right"
                      className="hidden font-mono tabular-nums xl:table-cell"
                    >
                      {(p.unique_users_30d ?? 0).toLocaleString('en-US')}
                      <div
                        className="text-ink-muted text-xs whitespace-nowrap"
                        title="Last activity"
                      >
                        {formatRelative(p.last_seen)}
                      </div>
                    </Td>
                  </LinkRow>
                );
              })}
            </TBody>
          </Table>
        </div>
        {inactive.length > 0 && (
          <div className="px-4 py-3">
            <Button
              variant="secondary"
              size="sm"
              aria-expanded={showInactive}
              onClick={() => setShowInactive((v) => !v)}
            >
              {showInactive
                ? `Hide ${inactive.length} inactive pools`
                : `Show ${inactive.length} inactive pool${inactive.length === 1 ? '' : 's'}`}
            </Button>
            {!showInactive && (
              <span className="text-ink-muted ml-3 text-xs">
                No readable state and no auctions.
              </span>
            )}
          </div>
        )}
      </Panel>
      {auctionsPanel}
    </>
  );
}

const COLS = 9;

/** Curated name, else "<Protocol> pool"; we don't invent pool names. */
function poolDisplayName(pool: string, protocol?: string | null) {
  const meta = LENDING_POOL_META[pool];
  return (
    meta?.name ??
    `${lendingProtocol(meta?.protocol ?? protocol ?? undefined)?.name ?? 'Lending'} pool`
  );
}

// A chart bar has no secondary line, so an uncurated pool keeps its short id to stay distinct.
function poolChartLabel(pool: string, protocol?: string | null) {
  return LENDING_POOL_META[pool]
    ? poolDisplayName(pool, protocol)
    : `${poolDisplayName(pool, protocol)} ${pool.slice(0, 4)}…${pool.slice(-4)}`;
}
