'use client';

import { useMemo, useState } from 'react';
import Link from 'next/link';
import { useQuery } from '@tanstack/react-query';

import { Panel } from '@/components/reveal';
import { AssetLink } from '@/components/AssetLink';
import {
  Badge,
  Button,
  Segmented,
  Skeleton,
  TBody,
  TR,
  Table,
  Td,
  THead,
} from '@/components/ui';
import { apiGet, asExample, type Envelope } from '@/api/client';
import type { paths } from '@/api/types';
import { shortAssetText } from '@/lib/asset-label';
import { SortableTh, useTableSort } from '@/lib/useTableSort';

import { LENDING_POOL_META, lendingProtocol } from '../poolMeta';
import { UsdFigure } from '../lendingUsd';

type RatesData = NonNullable<
  paths['/lending/rates']['get']['responses'][200]['content']['application/json']['data']
>;
export type LendingRate = NonNullable<RatesData['rates']>[number];

type SortKey = 'supply' | 'borrow' | 'supplied' | 'borrowed' | 'util' | 'asset';

const pct = (f: number | null | undefined) =>
  f == null ? '—' : `${(f * 100).toFixed(2)}%`;

// Sort-only: USD strings rank by magnitude; display stays exact.
const usdRank = (v: string | null | undefined) =>
  v == null ? null : Number(v);

const EXCLUSION_TEXT: Record<string, string> = {
  pool_not_active: 'pool not active',
  pool_status_unknown: 'pool status unknown',
  reserve_disabled: 'reserve disabled',
  reserve_config_unknown: 'reserve config unknown',
  rate_unavailable: 'rate unavailable',
};

const poolLabel = (pool: string, protocol?: string) =>
  LENDING_POOL_META[pool]?.name ??
  `${lendingProtocol(protocol)?.name ?? 'Lending'} pool ${pool.slice(0, 4)}…${pool.slice(-4)}`;

export const assetKey = (r: LendingRate) => r.asset_id ?? r.asset ?? '';

export function LendingRatesTable() {
  const [protocol, setProtocol] = useState<'all' | 'blend' | 'blnt'>('all');
  const [asset, setAsset] = useState('');
  const q = useQuery({
    queryKey: ['/v1/lending/rates'],
    staleTime: 60_000,
    queryFn: async () =>
      (await apiGet<Envelope<RatesData | null>>('/v1/lending/rates', {}))
        .data ?? null,
  });
  const all = useMemo(() => q.data?.rates ?? [], [q.data]);
  const assets = useMemo(() => {
    const m = new Map<string, string>();
    for (const r of all) m.set(assetKey(r), shortAssetText(assetKey(r)));
    return [...m.entries()].sort((a, b) => a[1].localeCompare(b[1]));
  }, [all]);
  const rows = all.filter(
    (r) =>
      (protocol === 'all' || r.protocol === protocol) &&
      (asset === '' || assetKey(r) === asset),
  );
  const { sorted, sort, toggle, ariaSort } = useTableSort<LendingRate, SortKey>(
    rows,
    [
      { key: 'supply', value: (r) => r.supply_apr },
      { key: 'borrow', value: (r) => r.borrow_apr },
      { key: 'supplied', value: (r) => usdRank(r.supplied_usd) },
      { key: 'borrowed', value: (r) => usdRank(r.borrowed_usd) },
      { key: 'util', value: (r) => r.utilization_pct },
      {
        key: 'asset',
        value: (r) => shortAssetText(assetKey(r)),
        initialDir: 'asc',
      },
    ],
    'supply',
  );
  const th = (
    label: string,
    key: SortKey,
    align: 'left' | 'right' = 'right',
  ) => (
    <SortableTh
      label={label}
      sortKey={key}
      sort={sort}
      onSort={toggle}
      ariaSort={ariaSort}
      align={align}
    />
  );
  const partial =
    q.data?.partial === true ||
    q.data?.truncated === true ||
    (q.data?.omitted_pools?.length ?? 0) > 0;

  return (
    <Panel
      headingLevel={2}
      title={`Reserves${rows.length > 0 ? ` (${rows.length})` : ''}`}
      hint="Current supply and borrow APR per reserve, read from each pool's storage. USD covers priced reserves only."
      source={asExample('/v1/lending/rates', {})}
      bodyClassName="-mx-4 -mb-4"
    >
      <div className="mx-4 mb-3 flex flex-wrap items-center gap-2">
        <Segmented
          ariaLabel="Protocol"
          value={protocol}
          onChange={(v) => setProtocol(v as typeof protocol)}
          options={(['all', 'blend', 'blnt'] as const).map((p) => ({
            value: p,
            label: p === 'all' ? 'All' : (lendingProtocol(p)?.name ?? p),
          }))}
        />
        <label className="text-ink-muted ml-auto text-xs">
          Asset{' '}
          <select
            className="border-line bg-surface text-ink ml-1 rounded border px-2 py-1 text-xs"
            value={asset}
            onChange={(e) => setAsset(e.target.value)}
          >
            <option value="">All assets</option>
            {assets.map(([id, label]) => (
              <option key={id} value={id}>
                {label}
              </option>
            ))}
          </select>
        </label>
      </div>
      {partial && (
        <p role="status" className="text-warn-700 mx-4 mb-2 text-xs">
          Partial list: some pools could not be read
          {(q.data?.omitted_pools?.length ?? 0) > 0
            ? ` (${q.data!.omitted_pools!.map((o) => poolLabel(o.pool ?? '', o.protocol)).join(', ')})`
            : ''}
          , so their reserves are missing below.
        </p>
      )}
      <div className="overflow-x-auto">
        <Table>
          <THead>
            <tr>
              {th('Asset', 'asset', 'left')}
              <th className="text-ink-muted px-3 py-2 text-left text-xs font-medium">
                Pool
              </th>
              {th('Supply APR', 'supply')}
              {th('Borrow APR', 'borrow')}
              {th('Supplied', 'supplied')}
              {th('Borrowed', 'borrowed')}
              {th('Utilisation', 'util')}
            </tr>
          </THead>
          <TBody>
            {q.isLoading && (
              <TR className="hover:bg-transparent">
                <Td colSpan={7}>
                  <Skeleton className="h-4 w-64" />
                </Td>
              </TR>
            )}
            {!q.isLoading && q.isError && (
              <TR className="hover:bg-transparent">
                <Td colSpan={7} className="text-ink-muted py-4">
                  Lending rates are unavailable right now.
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
                <Td colSpan={7} className="text-ink-muted py-4">
                  No reserves match this filter.
                </Td>
              </TR>
            )}
            {sorted.map((r) => {
              const pool = r.pool ?? '';
              const why = (r.exclusions ?? [])
                .map((e) => EXCLUSION_TEXT[e] ?? e)
                .join(', ');
              return (
                <TR key={`${pool}:${r.asset}`}>
                  <Td className="whitespace-nowrap" title={r.asset}>
                    <AssetLink canonical={assetKey(r)} />
                  </Td>
                  <Td className="whitespace-nowrap">
                    <Link
                      href={`/lending/${pool}`}
                      className="text-brand-600 text-xs hover:underline"
                      title={pool}
                    >
                      {poolLabel(pool, r.protocol)}
                    </Link>{' '}
                    <Badge>
                      {lendingProtocol(r.protocol)?.name ?? r.protocol}
                    </Badge>
                  </Td>
                  <Td
                    align="right"
                    className="text-ink font-mono font-medium tabular-nums"
                    title={why || undefined}
                  >
                    {pct(r.supply_apr)}
                    {r.obtainable === false && (
                      <span className="text-warn-500 ml-1 text-xs">
                        not obtainable
                      </span>
                    )}
                  </Td>
                  <Td align="right" className="font-mono tabular-nums">
                    {pct(r.borrow_apr)}
                  </Td>
                  <Td align="right" className="font-mono tabular-nums">
                    <UsdFigure
                      value={r.supplied_usd}
                      reason="no reliable price"
                    />
                  </Td>
                  <Td align="right" className="font-mono tabular-nums">
                    <UsdFigure
                      value={r.borrowed_usd}
                      reason="no reliable price"
                    />
                  </Td>
                  <Td align="right" className="font-mono tabular-nums">
                    {r.utilization_pct != null
                      ? `${r.utilization_pct.toFixed(1)}%`
                      : '—'}
                  </Td>
                </TR>
              );
            })}
          </TBody>
        </Table>
      </div>
    </Panel>
  );
}
