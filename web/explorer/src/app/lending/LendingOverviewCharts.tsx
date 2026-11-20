'use client';

import { Panel } from '@/components/reveal';
import { Button, Skeleton } from '@/components/ui';
import { HBarList, HATCH_BG, type HBarItem } from '@/components/charts/Bars';
import { asExample } from '@/api/client';
import { ratioPct, sumDecimalStrings } from '@/lib/format';

import { plotNumber, protocolColor, usdLabel } from './chartColors';
import { useLendingPools } from './LendingPoolsTable';
import {
  useLendingProtocols,
  type LendingProtocolRow,
} from './LendingProtocolStats';
import { LENDING_POOL_META, lendingProtocol } from './poolMeta';

type PoolRow = NonNullable<
  ReturnType<typeof useLendingPools>['data']
>['rows'][number];

type UsdLabel = NonNullable<ReturnType<typeof usdLabel>>;

export type ProtocolRow = {
  id: string;
  label: string;
  color: string;
  supplied: UsdLabel | null;
  borrowed: UsdLabel | null;
  /** Borrowed as a % of supplied, exact to 0.1; null when either is unpriced. */
  utilPct: number | null;
  /** Supplied as a % of every protocol's supplied total. */
  sharePct: number | null;
  lowerBound: boolean;
};

/**
 * One row per protocol with its own scale (supplied = 100%), so a protocol
 * 250× smaller still reads; the share-of-total bar carries the comparison.
 */
export function protocolRows(rows: LendingProtocolRow[]): ProtocolRow[] {
  const total = sumDecimalStrings(rows.map((r) => r.total_supplied_usd));
  return rows.flatMap((r) => {
    const lb = r.lower_bound === true;
    const supplied = usdLabel(r.total_supplied_usd, lb);
    const borrowed = usdLabel(r.total_borrowed_usd, lb);
    if (!supplied && !borrowed) return [];
    return [
      {
        id: r.protocol ?? '',
        label: lendingProtocol(r.protocol)?.name ?? r.protocol ?? 'Lending',
        color: protocolColor(r.protocol),
        supplied,
        borrowed,
        utilPct: ratioPct(r.total_borrowed_usd, r.total_supplied_usd, 1),
        sharePct: ratioPct(r.total_supplied_usd, total, 1),
        lowerBound: lb,
      },
    ];
  });
}

const poolLabel = (pool: string) =>
  LENDING_POOL_META[pool]?.name ?? `${pool.slice(0, 4)}…${pool.slice(-4)}`;

type TvlItem = HBarItem & { usd: string };

/** Pools ranked by TVL; pools with no USD TVL are counted, never drawn as $0. */
export function tvlBarItems(rows: PoolRow[]): {
  items: TvlItem[];
  unpriced: number;
} {
  const items: TvlItem[] = [];
  let unpriced = 0;
  for (const p of rows) {
    const s = p.summary;
    const v = plotNumber(s?.tvl_usd);
    const lbl = usdLabel(s?.tvl_usd, s?.lower_bound === true);
    // "≥$0" says nothing: a zero lower bound is unpriced, not empty.
    const zeroLb = v === 0 && s?.lower_bound === true;
    if (v == null || zeroLb || lbl == null || !p.pool || !s?.tvl_usd) {
      // A summary still being built is loading, not unpriced.
      if (!p.summary_pending) unpriced++;
      continue;
    }
    const proto = lendingProtocol(
      LENDING_POOL_META[p.pool]?.protocol ?? p.protocol,
    );
    items.push({
      id: p.pool,
      label: poolLabel(p.pool),
      value: v,
      usd: s.tvl_usd,
      display: lbl.display,
      annotation: proto?.name,
      color: protocolColor(LENDING_POOL_META[p.pool]?.protocol ?? p.protocol),
      hatchTail: s.lower_bound === true,
      title: `${p.pool} · ${lbl.exact}${s.lower_bound ? ' (priced reserves only)' : ''}`,
    });
  }
  items.sort((x, y) => y.value - x.value);
  return { items, unpriced };
}

/** The top `n` pools, then one "N more pools · $x" row summing the rest exactly. */
export function topPoolsWithTail(items: TvlItem[], n: number): HBarItem[] {
  if (items.length <= n + 1) return items;
  const tail = items.slice(n);
  const sum = sumDecimalStrings(tail.map((i) => i.usd));
  const lb = tail.some((i) => i.hatchTail);
  const lbl = usdLabel(sum, lb);
  return [
    ...items.slice(0, n),
    {
      id: '__tail',
      label: `${tail.length} more pools`,
      value: plotNumber(sum) ?? 0,
      display: lbl?.display,
      color: 'var(--color-chart-tail)',
      hatchTail: lb,
      title: `${lbl?.exact ?? ''} · ${tail.map((i) => i.label).join(', ')}`,
    },
  ];
}

function Unavailable({ what, retry }: { what: string; retry: () => void }) {
  return (
    <p role="alert" className="text-ink-muted text-sm">
      {what} is unavailable right now.
      <Button variant="secondary" size="sm" className="ml-3" onClick={retry}>
        Retry
      </Button>
    </p>
  );
}

function ProtocolBlock({ r }: { r: ProtocolRow }) {
  return (
    <li
      className="space-y-2"
      title={[r.supplied?.exact, r.borrowed?.exact].filter(Boolean).join(' · ')}
    >
      <div className="flex items-baseline justify-between gap-3">
        <span className="text-ink flex items-center gap-2 text-sm font-medium">
          <span
            aria-hidden
            className="inline-block h-2.5 w-2.5 rounded-xs"
            style={{ backgroundColor: r.color }}
          />
          {r.label}
        </span>
        {r.utilPct != null && (
          <span className="text-ink-muted text-xs">
            <span className="text-ink font-mono tabular-nums">
              {r.utilPct.toFixed(1)}%
            </span>{' '}
            borrowed
          </span>
        )}
      </div>
      <div className="flex flex-wrap gap-x-8 gap-y-1">
        {(
          [
            ['Supplied', r.supplied],
            ['Borrowed', r.borrowed],
          ] as const
        ).map(([k, v]) => (
          <div key={k}>
            <div className="text-ink-muted text-xs">{k}</div>
            <div className="text-ink font-mono text-xl font-medium tabular-nums">
              {v?.display ?? <span className="text-ink-faint">—</span>}
            </div>
          </div>
        ))}
      </div>
      {r.utilPct != null && (
        <div
          role="meter"
          aria-label={`${r.label}: ${r.utilPct.toFixed(1)}% of supplied is borrowed`}
          aria-valuemin={0}
          aria-valuemax={100}
          aria-valuenow={r.utilPct}
          className="relative h-3 w-full overflow-hidden rounded-xs"
        >
          <span
            aria-hidden
            className="absolute inset-0"
            style={{ backgroundColor: r.color, opacity: 0.3 }}
          />
          <span
            aria-hidden
            className="absolute inset-y-0 left-0"
            style={{
              width: `${Math.min(100, r.utilPct)}%`,
              backgroundColor: r.color,
            }}
          />
          {r.lowerBound && (
            <span
              aria-hidden
              className="text-ink-faint absolute inset-y-0 right-0 w-2 opacity-70"
              style={{ backgroundImage: HATCH_BG }}
            />
          )}
        </div>
      )}
    </li>
  );
}

function ShareBar({ rows }: { rows: ProtocolRow[] }) {
  const parts = rows.filter((r) => r.sharePct != null);
  if (parts.length < 2) return null;
  return (
    <div className="space-y-1.5">
      <div className="text-ink-muted text-xs">Share of total supplied</div>
      <div
        aria-label={`Share of total supplied: ${parts.map((r) => `${r.label} ${r.sharePct?.toFixed(1)}%`).join(', ')}`}
        role="img"
        className="flex h-2 w-full gap-px overflow-hidden rounded-xs"
      >
        {parts.map((r) => (
          <span
            key={r.id}
            className="min-w-[3px]"
            style={{ width: `${r.sharePct}%`, backgroundColor: r.color }}
          />
        ))}
      </div>
      <ul className="text-ink-muted flex flex-wrap gap-x-4 text-xs">
        {parts.map((r) => (
          <li key={r.id}>
            {r.label}{' '}
            <span className="text-ink font-mono tabular-nums">
              {r.sharePct! < 0.1 ? '<0.1' : r.sharePct!.toFixed(1)}%
            </span>
          </li>
        ))}
      </ul>
    </div>
  );
}

const TOP_POOLS = 5;

/**
 * LendingOverviewCharts — the /lending lead: where the money is (supplied vs
 * borrowed per protocol) and which pools hold it (TVL ranked by pool).
 */
export function LendingOverviewCharts() {
  const pq = useLendingProtocols();
  const lq = useLendingPools();
  const rows = protocolRows(pq.data ?? []);
  const anyLb = rows.some((r) => r.lowerBound);
  const { items, unpriced } = tvlBarItems(lq.data?.rows ?? []);
  const shown = topPoolsWithTail(items, TOP_POOLS);
  const anyPoolLb = items.some((i) => i.hatchTail);

  return (
    <div className="grid gap-4 lg:grid-cols-2">
      <Panel
        headingLevel={2}
        title="Supplied vs borrowed by protocol"
        hint="USD over priced reserves; each bar is the protocol's supply, filled to the share borrowed"
        source={asExample('/v1/lending/protocols', {})}
        bodyClassName="space-y-5"
      >
        {pq.isLoading && <Skeleton className="h-32 w-full" />}
        {pq.isError && (
          <Unavailable what="Protocol totals" retry={() => void pq.refetch()} />
        )}
        {pq.data && rows.length === 0 && (
          <p className="text-ink-muted text-sm">
            No priced lending totals yet.
          </p>
        )}
        {rows.length > 0 && (
          <ul
            aria-label={`Supplied and borrowed USD per protocol: ${rows
              .map(
                (r) =>
                  `${r.label} supplied ${r.supplied?.display ?? 'unpriced'}, borrowed ${r.borrowed?.display ?? 'unpriced'}`,
              )
              .join('; ')}`}
            className="space-y-5"
          >
            {rows.map((r) => (
              <ProtocolBlock key={r.id} r={r} />
            ))}
          </ul>
        )}
        <ShareBar rows={rows} />
        {anyLb && (
          <p className="text-ink-muted text-xs">
            ≥ marks a lower bound: unpriced or unreadable reserves are left out
            (listed under each protocol below).
          </p>
        )}
      </Panel>
      <Panel
        headingLevel={2}
        title="TVL by pool"
        hint="Supplied USD per pool, coloured by protocol"
        source={asExample('/v1/lending/pools', {})}
        bodyClassName="space-y-3"
      >
        {lq.isLoading && <Skeleton className="h-32 w-full" />}
        {lq.isError && (
          <Unavailable what="The pools list" retry={() => void lq.refetch()} />
        )}
        {lq.data && items.length === 0 && (
          <p className="text-ink-muted text-sm">
            No pool has a priced TVL yet.
          </p>
        )}
        {shown.length > 0 && (
          <HBarList
            items={shown}
            className="space-y-3 [&>li]:text-sm"
            ariaLabel={`TVL per pool: ${shown.map((i) => `${i.label} ${i.display}`).join(', ')}`}
          />
        )}
        {(unpriced > 0 || anyPoolLb) && (
          <p role="status" className="text-ink-muted text-xs">
            {unpriced > 0 &&
              `${unpriced} pool${unpriced === 1 ? '' : 's'} not priced, not shown. `}
            {anyPoolLb &&
              'Hatched ends (≥) are lower bounds over priced reserves only.'}
          </p>
        )}
      </Panel>
    </div>
  );
}
