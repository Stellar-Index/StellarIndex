'use client';

import Link from 'next/link';
import { useQuery } from '@tanstack/react-query';

import { Panel } from '@/components/reveal';
import { Button, TBody, TR, Table, Td, Th, THead } from '@/components/ui';
import { apiGet, asExample } from '@/api/client';
import type { paths } from '@/api/types';
import { ratioPct, sumDecimalStrings } from '@/lib/format';
import { shortAssetText } from '@/lib/asset-label';

import { type Envelope } from '../explorer-shared';
import { LENDING_POOL_META, lendingProtocol } from '../lending/poolMeta';
import { UsdFigure, exclusionText, reasonText } from '../lending/lendingUsd';
import { PairedBars, type PairedBarRow } from '@/components/charts/Bars';
import { plotNumber, protocolColor, usdLabel } from '../lending/chartColors';

export type LendingEarnings =
  paths['/lending/accounts/{account}/earnings']['get']['responses'][200]['content']['application/json']['data'];
type Leg = LendingEarnings['legs'][number];

const poolName = (p: string) =>
  LENDING_POOL_META[p]?.name ?? `pool ${p.slice(0, 6)}…${p.slice(-4)}`;

const aprText = (apr: string | null) => {
  const p = apr == null ? null : ratioPct(apr, '1');
  return p == null ? '—' : `${p.toFixed(2)}%`;
};

/** Accrued USD over a pool's legs, exact; a lower bound when an accrued leg has no USD. */
export function poolAccrued(legs: Leg[], pool: string) {
  const own = legs.filter((l) => l.pool === pool && l.accrued != null);
  return {
    usd: sumDecimalStrings(own.map((l) => l.accrued_usd)),
    lowerBound: own.some((l) => l.accrued_usd == null),
  };
}

/** Supplied vs borrowed USD per pool, coloured by protocol; exact figures on hover. */
export function positionMixRows(d: LendingEarnings): PairedBarRow[] {
  return d.pools.flatMap((p) => {
    const a = usdLabel(p.supplied_usd, p.lower_bound);
    const b = usdLabel(p.borrowed_usd, p.lower_bound);
    if (!a && !b) return [];
    return [
      {
        id: p.pool,
        label: poolName(p.pool),
        a: plotNumber(p.supplied_usd),
        b: plotNumber(p.borrowed_usd),
        aDisplay: a?.display,
        bDisplay: b?.display,
        aHatch: p.lower_bound && a != null,
        bHatch: p.lower_bound && b != null,
        color: protocolColor(p.protocol),
        title: `${p.pool} · ${[a && `supplied ${a.exact}`, b && `borrowed ${b.exact}`].filter(Boolean).join(', ')}`,
      },
    ];
  });
}

function PositionMix({ d }: { d: LendingEarnings }) {
  const rows = positionMixRows(d);
  const apr = d.totals.net_apr == null ? null : ratioPct(d.totals.net_apr, '1');
  return (
    <div className="space-y-2">
      <div className="flex flex-wrap items-baseline gap-x-4">
        <h3 className="text-ink text-sm font-medium">Position mix</h3>
        {apr != null && (
          <span
            className="text-ink-muted text-xs"
            title={d.totals.net_apr ?? undefined}
          >
            Net APR{' '}
            <span className="text-ink font-mono">
              <span aria-hidden>{apr >= 0 ? '▲' : '▼'}</span>{' '}
              {aprText(d.totals.net_apr)}
            </span>{' '}
            {apr >= 0 ? 'earning' : 'paying'} on net
          </span>
        )}
      </div>
      {rows.length > 0 && (
        <PairedBars
          rows={rows}
          aLabel="Supplied"
          bLabel="Borrowed"
          ariaLabel={`Supplied and borrowed USD per pool: ${rows
            .map(
              (r) =>
                `${r.label} supplied ${r.aDisplay ?? 'unpriced'}, borrowed ${r.bDisplay ?? 'unpriced'}`,
            )
            .join('; ')}`}
        />
      )}
    </div>
  );
}

/**
 * AccountLendingEarnings — the account's Blend and BLNT lending positions
 * valued in USD from /v1/lending/accounts/{a}/earnings. Unpriced legs are
 * listed with their reason, and the totals they leave out render as lower
 * bounds.
 */
export function AccountLendingEarnings({ id }: { id: string }) {
  const q = useQuery({
    queryKey: ['/v1/lending/accounts/{account}/earnings', id],
    enabled: id.length > 0,
    retry: false,
    staleTime: 30_000,
    queryFn: async () =>
      (
        await apiGet<Envelope<LendingEarnings>>(
          `/v1/lending/accounts/${encodeURIComponent(id)}/earnings`,
          {},
        )
      ).data,
  });
  const d = q.data;
  const legs = d?.legs ?? [];
  const unpriced = legs.filter((l) => l.value_usd == null && l.asset != null);
  const excluded = d?.totals.excluded ?? [];

  return (
    <Panel
      headingLevel={2}
      title="Lending positions"
      hint="Blend and BLNT supply and borrow positions valued in USD at current prices"
      source={asExample(`/v1/lending/accounts/${id}/earnings`, {})}
      bodyClassName="space-y-3"
    >
      {q.isLoading && (
        <p className="text-ink-muted text-sm">Loading lending positions…</p>
      )}
      {q.isError && (
        <p role="alert" className="text-ink-muted text-sm">
          Lending positions are unavailable right now.
          <Button
            variant="secondary"
            size="sm"
            className="ml-3"
            onClick={() => void q.refetch()}
          >
            Retry
          </Button>
        </p>
      )}
      {d && d.pools.length === 0 && legs.length === 0 && (
        <p className="text-ink-muted text-sm">
          This account has no Blend or BLNT lending history.
        </p>
      )}
      {d && d.pools.length > 0 && (
        <div className="overflow-x-auto">
          <Table>
            <THead>
              <tr>
                <Th>Pool</Th>
                <Th align="right">Supplied</Th>
                <Th align="right">Borrowed</Th>
                <Th align="right">Net</Th>
                <Th align="right" className="hidden sm:table-cell">
                  Net APR
                </Th>
                <Th align="right" className="hidden sm:table-cell">
                  Accrued
                </Th>
              </tr>
            </THead>
            <TBody>
              {d.pools.map((p) => {
                const acc = poolAccrued(legs, p.pool);
                return (
                  <TR key={p.pool}>
                    <Td className="whitespace-nowrap">
                      <Link
                        href={`/lending/${p.pool}`}
                        className="text-brand-600 text-xs hover:underline"
                        title={p.pool}
                      >
                        {poolName(p.pool)}
                      </Link>
                      <div className="text-ink-muted text-xs">
                        {lendingProtocol(p.protocol)?.name ?? p.protocol}
                      </div>
                    </Td>
                    <Td align="right" className="font-mono tabular-nums">
                      <UsdFigure
                        value={p.supplied_usd}
                        lowerBound={p.lower_bound}
                      />
                    </Td>
                    <Td align="right" className="font-mono tabular-nums">
                      <UsdFigure
                        value={p.borrowed_usd}
                        lowerBound={p.lower_bound}
                      />
                    </Td>
                    <Td
                      align="right"
                      className="text-ink font-mono tabular-nums"
                    >
                      <UsdFigure
                        value={p.net_usd}
                        reason={
                          p.net_usd_reason
                            ? 'some legs are unpriced, so net is unknown'
                            : undefined
                        }
                      />
                    </Td>
                    <Td
                      align="right"
                      className="hidden font-mono tabular-nums sm:table-cell"
                      title={p.net_apr_reason?.replace(/_/g, ' ') ?? undefined}
                    >
                      {aprText(p.net_apr)}
                    </Td>
                    <Td
                      align="right"
                      className="hidden font-mono tabular-nums sm:table-cell"
                    >
                      <UsdFigure
                        value={acc.usd}
                        lowerBound={acc.lowerBound}
                        reason={
                          acc.usd == null
                            ? 'no accrued interest valued'
                            : undefined
                        }
                      />
                    </Td>
                  </TR>
                );
              })}
              <TR className="font-medium">
                <Td>Total</Td>
                <Td align="right" className="font-mono tabular-nums">
                  <UsdFigure
                    value={d.totals.supplied_usd}
                    lowerBound={d.totals.lower_bound}
                  />
                </Td>
                <Td align="right" className="font-mono tabular-nums">
                  <UsdFigure
                    value={d.totals.borrowed_usd}
                    lowerBound={d.totals.lower_bound}
                  />
                </Td>
                <Td align="right" className="text-ink font-mono tabular-nums">
                  <UsdFigure
                    value={d.totals.net_usd}
                    reason="some legs are unpriced, so net is unknown"
                  />
                </Td>
                <Td
                  align="right"
                  className="hidden font-mono tabular-nums sm:table-cell"
                >
                  {aprText(d.totals.net_apr)}
                </Td>
                <Td className="hidden sm:table-cell" />
              </TR>
            </TBody>
          </Table>
        </div>
      )}
      {d && legs.filter((l) => l.value_usd != null).length >= 2 && (
        <PositionMix d={d} />
      )}
      {(unpriced.length > 0 || excluded.length > 0) && (
        <p role="status" className="text-ink-muted text-xs">
          Not valued:{' '}
          {[
            ...unpriced.map(
              (l) =>
                `${shortAssetText(l.asset)} ${l.side} in ${poolName(l.pool)} (${reasonText(l.usd_reason)})`,
            ),
            ...excluded
              .filter((e) => e.kind !== 'supply' && e.kind !== 'borrow')
              .map((e) => exclusionText(e, poolName)),
          ].join('; ')}
          . Totals marked ≥ leave these out.
        </p>
      )}
      {d?.coverage_note && (
        <p className="text-ink-muted text-xs">{d.coverage_note}</p>
      )}
    </Panel>
  );
}
