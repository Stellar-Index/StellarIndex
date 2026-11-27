'use client';

import { useState } from 'react';
import { useQuery } from '@tanstack/react-query';

import { Button, Skeleton, Stat, StatCell, StatGrid } from '@/components/ui';
import { apiGet, type Envelope } from '@/api/client';
import type { paths } from '@/api/types';
import { formatCompactTokenUnits } from '@/lib/format';
import { capWithheldNote } from '@/app/assets/[slug]/headerNotes';

import { LENDING_POOL_META } from './poolMeta';
import { UsdFigure, exclusionGroups, useAssetText } from './lendingUsd';

export type LendingProtocolRow = NonNullable<
  paths['/lending/protocols']['get']['responses'][200]['content']['application/json']['data']
>[number];

export function useLendingProtocols() {
  return useQuery({
    queryKey: ['/v1/lending/protocols'],
    staleTime: 60_000,
    queryFn: async () =>
      (
        await apiGet<Envelope<LendingProtocolRow[] | null>>(
          '/v1/lending/protocols',
          {},
        )
      ).data ?? [],
  });
}

const poolName = (p: string) =>
  LENDING_POOL_META[p]?.name ?? `pool ${p.slice(0, 6)}…${p.slice(-4)}`;

/**
 * LendingProtocolStats — one protocol's lending totals and token from
 * /v1/lending/protocols. The USD totals cover priced reserves only; a
 * partial total renders as a lower bound and names what it left out.
 */
export function LendingProtocolStats({
  protocol,
  compact = false,
}: {
  protocol: 'blend' | 'blnt';
  /** Supplied, borrowed and available only, without the token figures. */
  compact?: boolean;
}) {
  const q = useLendingProtocols();
  const [open, setOpen] = useState(false);
  if (q.isLoading) return <Skeleton className="h-16 w-full" />;
  if (q.isError)
    return (
      <p role="alert" className="text-ink-muted text-sm">
        Protocol totals are unavailable right now.
        <Button
          variant="secondary"
          size="sm"
          className="ml-3"
          onClick={() => void q.refetch()}
        >
          Retry
        </Button>
      </p>
    );
  const row = q.data?.find((r) => r.protocol === protocol);
  if (!row)
    return (
      <p className="text-ink-muted text-sm">
        No pool totals are served for this protocol yet.
      </p>
    );
  const lb = row.lower_bound === true;
  // backstop and token exclusions do not touch the USD totals.
  const left = (row.excluded ?? []).filter(
    (e) => e.kind !== 'backstop' && e.kind !== 'token',
  );
  const { line } = exclusionGroups(left, (e) => e.kind);
  const reason = lb
    ? `lower bound: ${line || 'some reserves left out'}`
    : undefined;
  const t = row.token;
  return (
    <div className="space-y-2">
      <StatGrid cols={3} className="grid-cols-2">
        <StatCell>
          <Stat
            label="Total supplied"
            value={
              <UsdFigure
                value={row.total_supplied_usd}
                lowerBound={lb}
                reason={reason}
              />
            }
            sub={`${row.pool_count ?? 0} pools · ${row.reserve_count ?? 0} reserves`}
          />
        </StatCell>
        <StatCell>
          <Stat
            label="Total borrowed"
            value={
              <UsdFigure
                value={row.total_borrowed_usd}
                lowerBound={lb}
                reason={reason}
              />
            }
          />
        </StatCell>
        <StatCell>
          <Stat
            label="Available liquidity"
            value={
              <UsdFigure
                value={row.available_usd}
                lowerBound={lb}
                reason={reason}
              />
            }
            sub="Supplied minus borrowed"
          />
        </StatCell>
        {!compact && <TokenCells t={t} />}
      </StatGrid>
      {lb && (
        <details
          className="text-ink-muted text-xs"
          onToggle={(e) => setOpen(e.currentTarget.open)}
        >
          <summary role="status" className="cursor-pointer">
            ≥ lower bound: {line || 'some reserves are left out'}
          </summary>
          {open && <ExcludedNames excluded={left} />}
        </details>
      )}
    </div>
  );
}

function TokenCells({ t }: { t: LendingProtocolRow['token'] }) {
  return (
    <>
      <StatCell>
        <Stat
          label={t?.code ? `${t.code} price` : 'Token price'}
          value={
            <UsdFigure
              value={t?.price_usd}
              reason={t ? 'no reliable market price yet' : 'token not verified'}
              showReason
            />
          }
          sub={t?.code ? <span title={t.asset_id}>{t.code}</span> : undefined}
        />
      </StatCell>
      <StatCell>
        <Stat
          label="Market cap"
          value={
            <UsdFigure
              value={t?.market_cap_usd}
              reason={
                t?.price_usd == null
                  ? 'no reliable market price yet'
                  : (capWithheldNote(t.market_cap_withheld_reason) ??
                    'not served yet')
              }
              showReason
            />
          }
        />
      </StatCell>
      <StatCell>
        <Stat
          label="Circulating supply"
          value={
            <span title={t?.circulating_supply ?? undefined}>
              {t?.circulating_supply != null
                ? formatCompactTokenUnits(t.circulating_supply, t.decimals)
                : '—'}
            </span>
          }
          sub={t?.code}
        />
      </StatCell>
    </>
  );
}

// Mounted only when the disclosure opens, so the asset-name map is not a first-paint request.
function ExcludedNames({
  excluded,
}: {
  excluded: NonNullable<LendingProtocolRow['excluded']>;
}) {
  const assetText = useAssetText();
  // One asset is often excluded from several pools; name it once.
  const { groups } = exclusionGroups(excluded, (e) =>
    e.asset ? assetText(e.asset) : e.pool ? poolName(e.pool) : e.kind,
  );
  return (
    <dl className="mt-2 space-y-1.5 pl-3">
      {groups.map((g) => (
        <div key={g.reason}>
          <dt className="text-ink-body inline">{g.reason}: </dt>
          <dd className="inline">{g.names.join(', ')}</dd>
        </div>
      ))}
    </dl>
  );
}
