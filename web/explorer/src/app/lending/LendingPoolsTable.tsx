'use client';

import Link from 'next/link';
import { useQuery } from '@tanstack/react-query';

import { Panel } from '@/components/reveal';
import {
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
import { apiGet, asExample } from '@/api/client';
import { formatRelative } from '@/lib/format';
import type { paths } from '@/api/types';

// One /v1/lending/pools row, derived from the generated OpenAPI
// contract (src/api/types.ts, `make web-generate-api`).
type LendingPool = NonNullable<
  paths['/lending/pools']['get']['responses'][200]['content']['application/json']['data']
>[number];

// Curated metadata for every Blend mainnet contract we know of.
// Sourced from docs/operations/wasm-audits/blend.md (Phase 4 walk).
interface PoolMeta {
  label: string;
  deployedAt?: string;
}

const BLEND_POOL_META: Record<string, PoolMeta> = {
  CAQQR5SWBXKIGZKPBZDH3KM5GQ5GUTPKB7JAFCINLZBC5WXPJKRG3IM7: {
    label: 'Backstop V2',
    deployedAt: '2025-04-14',
  },
  CDSYOAVXFY7SM5S64IZPPPYB4GVGGLMQVFREPSQQEZVIWXX5R23G4QSU: {
    label: 'Pool Factory V2',
    deployedAt: '2025-04-14',
  },
  CAJJZSGMMM3PD7N33TAPHGBUGTB43OC73HVIK2L2G6BNGGGYOSSYBXBD: {
    label: 'Pool #1 (genesis)',
    deployedAt: '2025-04-14',
  },
  CBNR7PYFY775UG7W37B4OJG2OBBUKLFW6VIBHFDKKLR2HECPRMRZMDK3: {
    label: 'Pool #2',
    deployedAt: '2025-04-15',
  },
  CCCCIQSDILITHMM7PBSLVDT5MISSY7R26MNZXCX4H7J5JQ5FPIYOGYFS: {
    label: 'Pool #3',
    deployedAt: '2025-04-17',
  },
  CB4OFHAY2TAEYUVPOJS36S657C6NYMSIFUNCCA5AHYT46Y5XUID3O2ED: {
    label: 'Pool #4',
    deployedAt: '2025-05-01',
  },
  CAE7QVOMBLZ53CDRGK3UNRRHG5EZ5NQA7HHTFASEMYBWHG6MDFZTYHXC: {
    label: 'Pool #5',
    deployedAt: '2025-05-01',
  },
  CBYOBT7ZCCLQCBUYYIABZLSEGDPEUWXCUXQTZYOG3YBDR7U357D5ZIRF: {
    label: 'Pool #6',
    deployedAt: '2025-07-13',
  },
  CALRF5I2OCJCU577R6MZBCY5IIXNMAAG6PNMN7GUKEYIXBJCJN2FJRVI: {
    label: 'Pool #7',
    deployedAt: '2025-11-22',
  },
  CADR6Q2UOCDJAGXMAB2E6SRT35STLZ2IGLZUCXJQG7TC2LNKCU5RTQVY: {
    label: 'Pool #8',
    deployedAt: '2025-11-25',
  },
  CDMAVJPFXPADND3YRL4BSM3AKZWCTFMX27GLLXCML3PD62HEQS5FPVAI: {
    label: 'Pool #9',
    deployedAt: '2025-11-25',
  },
};

// One /v1/lending/pools/{pool}/reserves reserve row (the fields this table uses).
interface PoolReserve {
  supplied_usd?: string;
  borrowed_usd?: string;
  supply_apr?: number;
  borrow_apr?: number;
}

// parseUsd — a served USD decimal string → finite number, or null when the
// field is absent/garbage. Absence means UNPRICED, never zero: coercing it
// to 0 inside sums and weighted averages fabricated pool-level percentages.
function parseUsd(s: string | undefined): number | null {
  if (s == null || s === '') return null;
  const n = Number(s);
  return Number.isFinite(n) ? n : null;
}

// sumPresent — sum a field over the reserves where it is actually served;
// null when NO reserve carries it (an all-absent basis is not a zero).
function sumPresent(
  reserves: PoolReserve[],
  pick: (r: PoolReserve) => number | null,
): {
  sum: number | null;
  count: number;
} {
  let sum = 0;
  let count = 0;
  for (const r of reserves) {
    const v = pick(r);
    if (v == null) continue;
    sum += v;
    count += 1;
  }
  return { sum: count > 0 ? sum : null, count };
}

// weightedAvg — weight-averaged rate over the reserves where BOTH the
// weight and the rate are served; null when the priced weight basis is 0.
function weightedAvg(
  reserves: PoolReserve[],
  weight: (r: PoolReserve) => number | null,
  rate: (r: PoolReserve) => number | undefined,
): number | null {
  let num = 0;
  let den = 0;
  for (const r of reserves) {
    const w = weight(r);
    const a = rate(r);
    if (w == null || typeof a !== 'number' || !Number.isFinite(a)) continue;
    num += w * a;
    den += w;
  }
  return den > 0 ? num / den : null;
}

/**
 * PoolRealStats — TVL (USD) + true utilization from the pool-storage
 * reader via /v1/lending/pools/{pool}/reserves (window event proxies in raw
 * base-units — "578.1T supplied", "222.4% util" — would be impossible
 * numbers presented as real ones while the real ones exist one endpoint
 * over).
 *
 * All derived figures are computed over PRICED reserves only — an
 * unpriced reserve is excluded from the basis, never counted as $0
 * (that coerced-zero fabricated the percentages) — and each cell's
 * title labels that basis. No priced reserve at all renders "—".
 * Module-scope (not nested in LendingPoolsTable) so React keeps one
 * component identity across parent re-renders.
 */
function PoolRealStats({ pool }: { pool: string }) {
  const q = useQuery<{
    tvl_usd?: string;
    reserves?: PoolReserve[];
  }>({
    queryKey: ['/v1/lending/pools/{pool}/reserves', pool],
    staleTime: 300_000,
    retry: false,
    queryFn: async () =>
      (
        await apiGet<{ data: { tvl_usd?: string; reserves?: PoolReserve[] } }>(
          `/v1/lending/pools/${encodeURIComponent(pool)}/reserves`,
          {},
        )
      ).data,
  });
  const reserves = q.data?.reserves ?? [];
  const tvl = parseUsd(q.data?.tvl_usd);
  const supplied = sumPresent(reserves, (r) => parseUsd(r.supplied_usd));
  const borrowed = sumPresent(reserves, (r) => parseUsd(r.borrowed_usd));
  const util =
    supplied.sum != null && supplied.sum > 0 && borrowed.sum != null
      ? (borrowed.sum / supplied.sum) * 100
      : null;
  // BACKLOG #30: supplied-weighted average APRs from the same reserves
  // the row already fetched — no extra request.
  const wSupplyAPR = weightedAvg(
    reserves,
    (r) => parseUsd(r.supplied_usd),
    (r) => r.supply_apr,
  );
  const wBorrowAPR = weightedAvg(
    reserves,
    (r) => parseUsd(r.borrowed_usd),
    (r) => r.borrow_apr,
  );
  const basisNote = (count: number) =>
    reserves.length > 0 && count < reserves.length
      ? `Priced reserves only (${count} of ${reserves.length})`
      : 'Priced reserves only';
  const fmtUsd = (n: number) =>
    n >= 1e9
      ? `$${(n / 1e9).toFixed(2)}B`
      : n >= 1e6
        ? `$${(n / 1e6).toFixed(2)}M`
        : `$${Math.round(n).toLocaleString('en-US')}`;
  const cell = (v: number | null, fmt: (n: number) => string) =>
    q.isLoading ? '…' : v != null ? fmt(v) : '—';
  const pctFmt = (n: number) => `${(n * 100).toFixed(2)}%`;
  return (
    <>
      <Td align="right" className="font-mono tabular-nums">
        {cell(tvl, fmtUsd)}
      </Td>
      <Td
        align="right"
        className="hidden font-mono tabular-nums sm:table-cell"
        title={
          util != null
            ? basisNote(Math.min(supplied.count, borrowed.count))
            : undefined
        }
      >
        {cell(util, (n) => `${n.toFixed(1)}%`)}
      </Td>
      <Td
        align="right"
        className="hidden font-mono tabular-nums sm:table-cell"
        title={wSupplyAPR != null ? basisNote(supplied.count) : undefined}
      >
        {cell(wSupplyAPR, pctFmt)}
      </Td>
      <Td
        align="right"
        className="hidden font-mono tabular-nums sm:table-cell"
        title={wBorrowAPR != null ? basisNote(borrowed.count) : undefined}
      >
        {cell(wBorrowAPR, pctFmt)}
      </Td>
    </>
  );
}

export function LendingPoolsTable() {
  const q = useQuery<LendingPool[]>({
    queryKey: ['/v1/lending/pools'],
    queryFn: async () => {
      const env = await apiGet<{ data: LendingPool[] }>(
        '/v1/lending/pools',
        {},
      );
      return env.data ?? [];
    },
  });

  const rows = q.data ?? [];

  // Liquidation-auction distribution across pools — same fetched rows,
  // no extra request. Renders only when at least one pool has auctions.
  const auctionRows = rows
    .filter((p) => (p.auctions_total ?? 0) > 0)
    .sort((x, y) => (y.auctions_total ?? 0) - (x.auctions_total ?? 0));

  return (
    <>
      {auctionRows.length > 0 && (
        <Panel
          title="Liquidation auctions by pool — all time"
          hint="Auction events from the indexed Blend event stream; the risk view of which pools actually liquidate."
          source={asExample('/v1/lending/pools', {})}
        >
          <HBarList
            ariaLabel={`All-time liquidation auctions per pool: ${auctionRows
              .map(
                (p) =>
                  `${BLEND_POOL_META[p.pool ?? '']?.label ?? p.pool} ${(p.auctions_total ?? 0).toLocaleString('en-US')}`,
              )
              .join(', ')}`}
            items={auctionRows.map((p) => ({
              label:
                BLEND_POOL_META[p.pool ?? '']?.label ??
                `${(p.pool ?? '').slice(0, 6)}…${(p.pool ?? '').slice(-4)}`,
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
      )}

      <Panel
        title={`Pools${rows.length > 0 ? ` (${rows.length})` : ''}`}
        hint="TVL and utilization read live from pool storage; auctions and users from the indexed event stream"
        source={asExample('/v1/lending/pools', {})}
        bodyClassName="-mx-4 -mb-4"
      >
        <div className="overflow-x-auto">
          <Table>
            <THead>
              <tr>
                <Th>Pool</Th>
                <Th className="hidden xl:table-cell">Deployed</Th>
                <Th align="right" className="hidden xl:table-cell">
                  Auctions 24h
                </Th>
                <Th align="right">Auctions</Th>
                <Th align="right">TVL</Th>
                <Th align="right" className="hidden sm:table-cell">
                  Utilization
                </Th>
                <Th align="right" className="hidden sm:table-cell">
                  Supply APR
                </Th>
                <Th align="right" className="hidden sm:table-cell">
                  Borrow APR
                </Th>
                <Th align="right" className="hidden xl:table-cell">
                  Users (30d)
                </Th>
                <Th align="right" className="hidden xl:table-cell">
                  Last activity
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
                    No Blend pools have emitted auction events yet.
                  </Td>
                </TR>
              )}
              {rows.map((p) => {
                const poolId = p.pool ?? '';
                const meta = BLEND_POOL_META[poolId];
                return (
                  <LinkRow href={`/lending/${poolId}`} key={poolId}>
                    <Td>
                      <div className="flex items-center gap-1">
                        <Link
                          href={`/lending/${poolId}`}
                          className="text-brand-600 font-mono text-xs whitespace-nowrap hover:underline"
                          title={poolId}
                        >
                          {poolId.slice(0, 6)}…{poolId.slice(-6)}
                        </Link>
                        <CopyButton value={poolId} />
                      </div>
                      {/* Curated label where we have one; we don't invent pool names. */}
                      <div className="text-ink-muted text-xs whitespace-nowrap">
                        {meta?.label ?? 'Blend pool'}
                      </div>
                    </Td>
                    <Td className="hidden font-mono text-xs whitespace-nowrap xl:table-cell">
                      {meta?.deployedAt ?? (
                        <span className="text-ink-faint">—</span>
                      )}
                    </Td>
                    <Td
                      align="right"
                      className="hidden font-mono tabular-nums xl:table-cell"
                    >
                      {(p.auctions_24h ?? 0).toLocaleString('en-US')}
                    </Td>
                    <Td align="right" className="font-mono tabular-nums">
                      {(p.auctions_total ?? 0).toLocaleString('en-US')}
                    </Td>
                    <PoolRealStats pool={poolId} />
                    <Td
                      align="right"
                      className="hidden font-mono tabular-nums xl:table-cell"
                    >
                      {(p.unique_users_30d ?? 0).toLocaleString('en-US')}
                    </Td>
                    <Td
                      align="right"
                      className="text-ink-muted hidden text-xs whitespace-nowrap xl:table-cell"
                    >
                      {formatRelative(p.last_seen)}
                    </Td>
                  </LinkRow>
                );
              })}
            </TBody>
          </Table>
        </div>
      </Panel>
    </>
  );
}

const COLS = 10;
