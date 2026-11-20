'use client';

import { Panel } from '@/components/reveal';
import { Button, Skeleton } from '@/components/ui';
import {
  HBarList,
  PairedBars,
  type HBarItem,
  type PairedBarRow,
} from '@/components/charts/Bars';
import { asExample } from '@/api/client';

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

/** Supplied vs borrowed USD per protocol; a protocol with neither priced is dropped. */
export function protocolPairRows(rows: LendingProtocolRow[]): PairedBarRow[] {
  return rows.flatMap((r) => {
    const lb = r.lower_bound === true;
    const a = usdLabel(r.total_supplied_usd, lb);
    const b = usdLabel(r.total_borrowed_usd, lb);
    if (!a && !b) return [];
    return [
      {
        id: r.protocol ?? '',
        label: lendingProtocol(r.protocol)?.name ?? r.protocol ?? 'Lending',
        a: plotNumber(r.total_supplied_usd),
        b: plotNumber(r.total_borrowed_usd),
        aDisplay: a?.display,
        bDisplay: b?.display,
        aHatch: lb && a != null,
        bHatch: lb && b != null,
        color: protocolColor(r.protocol),
        title: [
          a?.exact && `Supplied ${a.exact}`,
          b?.exact && `Borrowed ${b.exact}`,
        ]
          .filter(Boolean)
          .join(' · '),
      },
    ];
  });
}

const poolLabel = (pool: string) =>
  LENDING_POOL_META[pool]?.name ?? `${pool.slice(0, 4)}…${pool.slice(-4)}`;

/** Pools ranked by TVL; pools with no USD TVL are counted, never drawn as $0. */
export function tvlBarItems(rows: PoolRow[]): {
  items: HBarItem[];
  unpriced: number;
} {
  const items: (HBarItem & { value: number })[] = [];
  let unpriced = 0;
  for (const p of rows) {
    const s = p.summary;
    const v = plotNumber(s?.tvl_usd);
    const lbl = usdLabel(s?.tvl_usd, s?.lower_bound === true);
    if (v == null || lbl == null || !p.pool) {
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
      display: lbl.display,
      annotation: proto?.name,
      color: protocolColor(LENDING_POOL_META[p.pool]?.protocol ?? p.protocol),
      hatchTail: s?.lower_bound === true,
      title: `${p.pool} · ${lbl.exact}${s?.lower_bound ? ' (priced reserves only)' : ''}`,
    });
  }
  items.sort((x, y) => y.value - x.value);
  return { items, unpriced };
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

/**
 * LendingOverviewCharts — the /lending lead: where the money is (supplied vs
 * borrowed per protocol) and which pools hold it (TVL ranked by pool).
 */
export function LendingOverviewCharts() {
  const pq = useLendingProtocols();
  const lq = useLendingPools();
  const pairs = protocolPairRows(pq.data ?? []);
  const anyLb = (pq.data ?? []).some((r) => r.lower_bound);
  const { items, unpriced } = tvlBarItems(lq.data?.rows ?? []);
  const anyPoolLb = items.some((i) => i.hatchTail);

  return (
    <div className="grid gap-4 lg:grid-cols-2">
      <Panel
        headingLevel={2}
        title="Supplied vs borrowed by protocol"
        hint="USD over priced reserves"
        source={asExample('/v1/lending/protocols', {})}
        bodyClassName="space-y-2"
      >
        {pq.isLoading && <Skeleton className="h-16 w-full" />}
        {pq.isError && (
          <Unavailable what="Protocol totals" retry={() => void pq.refetch()} />
        )}
        {pq.data && pairs.length === 0 && (
          <p className="text-ink-muted text-sm">
            No priced lending totals yet.
          </p>
        )}
        {pairs.length > 0 && (
          <PairedBars
            rows={pairs}
            aLabel="Supplied"
            bLabel="Borrowed"
            ariaLabel={`Supplied and borrowed USD per protocol: ${pairs
              .map(
                (r) =>
                  `${r.label} supplied ${r.aDisplay ?? 'unpriced'}, borrowed ${r.bDisplay ?? 'unpriced'}`,
              )
              .join('; ')}`}
          />
        )}
        {anyLb && (
          <p className="text-ink-muted text-xs">
            ≥ marks a lower bound: unpriced or unreadable reserves are left out
            (named under each protocol below).
          </p>
        )}
      </Panel>
      <Panel
        headingLevel={2}
        title="TVL by pool"
        hint="Supplied USD per pool, coloured by protocol"
        source={asExample('/v1/lending/pools', {})}
        bodyClassName="space-y-2"
      >
        {lq.isLoading && <Skeleton className="h-24 w-full" />}
        {lq.isError && (
          <Unavailable what="The pools list" retry={() => void lq.refetch()} />
        )}
        {lq.data && items.length === 0 && (
          <p className="text-ink-muted text-sm">
            No pool has a priced TVL yet.
          </p>
        )}
        {items.length > 0 && (
          <HBarList
            items={items}
            ariaLabel={`TVL per pool: ${items.map((i) => `${i.label} ${i.display}`).join(', ')}`}
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
