'use client';

import { useState } from 'react';
import { useQuery } from '@tanstack/react-query';

import { Panel } from '@/components/reveal';
import { StackedColumns } from '@/components/charts/Bars';
import {
  Button,
  Segmented,
  Stat,
  StatCell,
  StatGrid,
  TBody,
  TR,
  Table,
  Td,
  Th,
  THead,
} from '@/components/ui';
import { apiGet, asExample, type Envelope } from '@/api/client';
import type { components, paths } from '@/api/types';
import { formatCompact, sumDecimalStrings } from '@/lib/format';
import { plotNumber } from '../chartColors';
import {
  amountText,
  isHistoryBuilding,
  RANGES,
  type RangeOption,
} from '../lendingInsights';
import { revenueReasonText } from '../lendingRevenueText';
import { UsdFigure, useAssetText } from '../lendingUsd';

export type Revenue =
  paths['/lending/pools/{pool}/revenue']['get']['responses'][200]['content']['application/json']['data'];
type Reserve = Revenue['reserves'][number];
type Usd = components['schemas']['LendingRevenueUSD'];

const LEGS = [
  { key: 'supplier', label: 'Supplier share', color: 'var(--color-chart-2)' },
  { key: 'backstop', label: 'Backstop take', color: 'var(--color-chart-4)' },
  { key: 'fee', label: 'Protocol fee', color: 'var(--color-chart-5)' },
] as const;

const isZero = (v: string | null | undefined) =>
  v != null && /^-?0+(\.0+)?$/.test(v);

/** Every reserve null: recomputing (a stale or pending derive) or not derived at all. */
export function revenueState(d: Revenue): 'ok' | 'recomputing' | 'not_derived' {
  if (d.reserves.some((r) => r.totals != null) || d.excluded.length === 0)
    return 'ok';
  return d.excluded.some(
    (e) => e.reason === 'derive_stale' || e.reason === 'redo_pending',
  )
    ? 'recomputing'
    : 'not_derived';
}

/** One line per thing a lower bound leaves out, base-unit exclusions first. */
export function exclusionNotes(
  d: Revenue,
  assetText: (a: string) => string,
): string[] {
  const dec = new Map(d.reserves.map((r) => [r.asset, r.decimals]));
  const notes = d.excluded.map((e) => {
    const what = e.asset
      ? assetText(e.asset)
      : e.kind === 'window'
        ? 'Part of this range'
        : 'This pool';
    const unit =
      e.reason === 'derive_stale' || e.reason === 'redo_pending'
        ? 'hours'
        : 'rate changes';
    const count =
      e.count != null ? `${e.count.toLocaleString('en-US')} ${unit}` : null;
    const amount =
      e.amount != null && e.asset
        ? `${amountText(e.amount, dec.get(e.asset))} ${assetText(e.asset)}`
        : null;
    const detail = [amount, count].filter(Boolean).join(' over ');
    return `${what}: ${revenueReasonText(e.reason)}${detail ? ` (${detail})` : ''}`;
  });
  for (const e of d.totals_usd.excluded)
    notes.push(
      `${assetText(e.asset)}: ${revenueReasonText(e.reason)} (${e.count.toLocaleString('en-US')} hours not valued)`,
    );
  return notes;
}

export interface RevenueBucket {
  t: string;
  supplier: string | null;
  backstop: string | null;
  fee: string | null;
  partial: boolean;
  /** Some interest in the bucket has no USD value. */
  lowerBound: boolean;
}

/** The pool's accrued interest per bucket and leg, summed exactly across reserves. */
export function revenueBuckets(reserves: Reserve[]): RevenueBucket[] {
  const by = new Map<
    string,
    { s: string[]; b: string[]; f: string[]; partial: boolean; lb: boolean }
  >();
  for (const r of reserves)
    for (const p of r.points) {
      let b = by.get(p.t);
      if (!b)
        by.set(p.t, (b = { s: [], b: [], f: [], partial: false, lb: false }));
      b.partial ||= p.partial;
      const u = p.usd;
      if (u.interest_accrued_usd == null) {
        if (!isZero(p.interest_accrued)) b.lb = true;
        continue;
      }
      b.lb ||= u.lower_bound;
      if (u.supplier_share_usd != null) b.s.push(u.supplier_share_usd);
      if (u.backstop_take_usd != null) b.b.push(u.backstop_take_usd);
      if (u.protocol_fee_usd != null) b.f.push(u.protocol_fee_usd);
    }
  return [...by]
    .sort(([a], [b]) => (a < b ? -1 : a > b ? 1 : 0))
    .map(([t, b]) => ({
      t,
      supplier: sumDecimalStrings(b.s),
      backstop: sumDecimalStrings(b.b),
      fee: sumDecimalStrings(b.f),
      partial: b.partial,
      lowerBound: b.lb,
    }));
}

function basisText(u: Usd): string {
  const basis =
    u.price_basis_hours && Object.keys(u.price_basis_hours).length > 0
      ? 'mixed'
      : u.price_basis === 'transitive'
        ? 'via XLM'
        : u.price_basis === 'stablecoin_proxy'
          ? 'stablecoin book'
          : u.priced_hours > 0
            ? 'USD market'
            : 'not priced';
  return u.price_carried_hours > 0
    ? `${basis}, ${u.price_carried_hours.toLocaleString('en-US')} h carried`
    : basis;
}

function statusText(r: Reserve, d: Revenue): string {
  if (r.totals == null) {
    const e =
      d.excluded.find((x) => x.asset === r.asset) ??
      d.excluded.find((x) => x.asset == null);
    return e ? revenueReasonText(e.reason) : 'nothing summed';
  }
  const parts: string[] = [];
  if (r.usd.unpriced_hours > 0)
    parts.push(`${r.usd.unpriced_hours.toLocaleString('en-US')} h unpriced`);
  else if (r.lower_bound || r.usd.lower_bound) parts.push('lower bound');
  if (!isZero(r.totals.unexplained_supply_loss))
    parts.push('unexplained supplier loss');
  if (!isZero(r.totals.default_loss)) parts.push('default loss');
  return parts.join(', ') || 'complete';
}

function Losses({
  d,
  assetText,
}: {
  d: Revenue;
  assetText: (a: string) => string;
}) {
  const unexplained = d.reserves.filter(
    (r) => r.totals && !isZero(r.totals.unexplained_supply_loss),
  );
  const t = d.totals_usd;
  return (
    <p className="text-ink-muted text-xs" data-testid="revenue-losses">
      Losses, shown apart and never netted from the interest above: default
      losses socialised to suppliers{' '}
      <UsdFigure
        value={t.default_loss_usd}
        lowerBound={t.lower_bound}
        reason={
          t.default_loss_usd == null
            ? revenueReasonText(t.usd_reason)
            : undefined
        }
      />
      {unexplained.length > 0 ? (
        <>
          ; supplier loss no event explains:{' '}
          {unexplained.map((r, i) => (
            <span key={r.asset} title={r.asset}>
              {i > 0 && ', '}
              {amountText(r.totals!.unexplained_supply_loss, r.decimals)}{' '}
              {assetText(r.asset)} over{' '}
              {r.totals!.unexplained_supply_loss_versions.toLocaleString(
                'en-US',
              )}{' '}
              rate changes
            </span>
          ))}
        </>
      ) : (
        '; no supplier loss without an explaining event'
      )}
      .
    </p>
  );
}

function RevenueBody({ d, hourly }: { d: Revenue; hourly: boolean }) {
  const assetText = useAssetText();
  const state = revenueState(d);
  if (state === 'recomputing')
    return (
      <p role="status" className="text-ink-muted text-sm">
        Revenue history is being recomputed. Check back soon.
      </p>
    );
  if (state === 'not_derived')
    return (
      <p role="status" className="text-ink-muted text-sm">
        Revenue cannot be shown yet:{' '}
        {d.excluded.map((e) => revenueReasonText(e.reason)).join('; ')}.
      </p>
    );
  const blnt = d.protocol === 'blnt';
  const legs = LEGS.filter((l) => blnt || l.key !== 'fee');
  const notes = exclusionNotes(d, assetText);
  const t = d.totals_usd;
  const lbReason = notes.length ? `leaves out ${notes.join('; ')}` : undefined;
  const fig = (v: string | null, lb = t.lower_bound) => (
    <UsdFigure
      value={v}
      lowerBound={lb}
      reason={v == null ? revenueReasonText(t.usd_reason) : lbReason}
      showReason
    />
  );
  const feeInferred = d.reserves.some((r) => r.fee_rate_inferred);
  const feeWithdrawn = d.reserves.find(
    (r) => r.totals && r.totals.realised.fee_withdrawn == null,
  )?.totals?.realised.fee_withdrawn_reason;
  const buckets = revenueBuckets(d.reserves);
  const label = (b: RevenueBucket) =>
    `${hourly ? `${b.t.slice(0, 10)} ${b.t.slice(11, 16)}` : b.t.slice(0, 10)}${b.partial ? ' (partial)' : ''}${b.lowerBound ? ' (≥)' : ''}`;
  const unvalued = buckets.filter((b) => b.lowerBound).length;
  const partial = buckets.filter((b) => b.partial).length;

  return (
    <>
      <StatGrid cols={blnt ? 5 : 4} className="grid-cols-2">
        <StatCell>
          <Stat
            label="Interest accrued"
            value={fig(t.interest_accrued_usd)}
            sub="USD, this range"
          />
        </StatCell>
        <StatCell>
          <Stat
            label="Supplier share"
            value={fig(t.supplier_share_usd)}
            sub="Paid to suppliers"
          />
        </StatCell>
        <StatCell>
          <Stat
            label="Backstop take"
            value={fig(t.backstop_take_usd)}
            sub="Credited to the backstop"
          />
        </StatCell>
        {blnt && (
          <StatCell>
            <Stat
              label="Protocol fee"
              value={fig(t.protocol_fee_usd)}
              sub={feeInferred ? 'Fee rate inferred' : 'BLNT fee'}
            />
          </StatCell>
        )}
        <StatCell>
          <Stat
            label="Paid out to backstop"
            value={
              <UsdFigure
                value={t.backstop_drawn_usd}
                lowerBound
                reason={
                  t.backstop_drawn_usd == null
                    ? revenueReasonText(t.usd_reason)
                    : 'fills that state no lot are left out'
                }
              />
            }
            sub="At least; interest auctions"
          />
        </StatCell>
      </StatGrid>
      {notes.length > 0 && (
        <div className="text-ink-muted text-xs" data-testid="revenue-excluded">
          ≥ marks a lower bound (the true figure is at least the one shown). It
          leaves out:
          <ul className="mt-1 list-disc pl-5">
            {notes.map((n) => (
              <li key={n}>{n}</li>
            ))}
          </ul>
        </div>
      )}
      <Losses d={d} assetText={assetText} />
      {blnt && feeWithdrawn && (
        <p className="text-ink-muted text-xs">
          Protocol fee withdrawn: {revenueReasonText(feeWithdrawn)}.
        </p>
      )}
      {buckets.length > 0 && (
        <div className="space-y-2">
          <h3 className="text-ink-body text-xs font-medium">
            Interest accrued per {hourly ? 'hour' : 'day'}, by who receives it
          </h3>
          <StackedColumns
            buckets={buckets.map((b) => ({
              label: label(b),
              values: legs.map((l) => plotNumber(b[l.key]) ?? 0),
            }))}
            series={legs.map((l) => ({ label: l.label, color: l.color }))}
            formatValue={(n) => `$${formatCompact(n)}`}
            ariaLabel={`Interest accrued in USD per ${hourly ? 'hour' : 'day'} by leg, ${buckets.length} buckets`}
          />
          {(partial > 0 || unvalued > 0) && (
            <p className="text-ink-muted text-xs">
              {partial > 0 &&
                `${partial} ${partial === 1 ? 'bucket is' : 'buckets are'} partial: cut short by the range end or by what is derived so far. `}
              {unvalued > 0 &&
                `${unvalued} ${unvalued === 1 ? 'bucket has' : 'buckets have'} interest with no USD value, so the column shown is a lower bound.`}
            </p>
          )}
        </div>
      )}
      {d.reserves.length > 0 && (
        <div className="overflow-x-auto">
          <Table>
            <THead>
              <tr>
                <Th>Asset</Th>
                <Th align="right">Accrued</Th>
                <Th align="right">Supplier share</Th>
                <Th align="right">Backstop take</Th>
                {blnt && <Th align="right">Protocol fee</Th>}
                <Th className="hidden sm:table-cell">Price basis</Th>
                <Th>Status</Th>
              </tr>
            </THead>
            <TBody>
              {d.reserves.map((r) => {
                const lb = r.lower_bound || r.usd.lower_bound;
                const cell = (v: string | null) => (
                  <UsdFigure
                    value={v}
                    lowerBound={lb}
                    reason={
                      v == null
                        ? r.usd.usd_reason
                          ? revenueReasonText(r.usd.usd_reason)
                          : statusText(r, d)
                        : undefined
                    }
                  />
                );
                return (
                  <TR key={r.asset}>
                    <Td title={r.asset}>{assetText(r.asset)}</Td>
                    <Td align="right" className="tabular-nums">
                      {cell(r.usd.interest_accrued_usd)}
                    </Td>
                    <Td align="right" className="tabular-nums">
                      {cell(r.usd.supplier_share_usd)}
                    </Td>
                    <Td align="right" className="tabular-nums">
                      {cell(r.usd.backstop_take_usd)}
                    </Td>
                    {blnt && (
                      <Td align="right" className="tabular-nums">
                        {cell(r.usd.protocol_fee_usd)}
                      </Td>
                    )}
                    <Td className="text-ink-muted hidden text-xs sm:table-cell">
                      {basisText(r.usd)}
                    </Td>
                    <Td
                      className="text-ink-muted text-xs"
                      data-testid={`revenue-status-${r.asset}`}
                    >
                      {statusText(r, d)}
                    </Td>
                  </TR>
                );
              })}
            </TBody>
          </Table>
        </div>
      )}
      <p className="text-ink-muted text-xs">
        Each hour is valued at its own USD price, at most{' '}
        {d.price_max_age_hours} hours old
        {d.coverage.through_hour &&
          `; derived through ${d.coverage.through_hour.replace('T', ' ').slice(0, 16)} UTC`}
        .
      </p>
    </>
  );
}

export function RevenuePanel({ pool }: { pool: string }) {
  const [rangeId, setRangeId] = useState<RangeOption['id']>('90d');
  const range = RANGES.find((r) => r.id === rangeId) ?? RANGES[1];
  const q = useQuery({
    queryKey: ['/v1/lending/pools/{pool}/revenue', pool, range.id],
    retry: false,
    staleTime: 300_000,
    queryFn: async () =>
      (
        await apiGet<Envelope<Revenue>>(
          `/v1/lending/pools/${encodeURIComponent(pool)}/revenue`,
          { interval: range.interval, window: range.window },
        )
      ).data,
  });
  // Not a pool the API derives interest for (a curated factory or backstop contract).
  if (q.error instanceof Error && q.error.message.startsWith('404'))
    return null;
  return (
    <Panel
      headingLevel={2}
      title="Interest & revenue"
      hint="Interest borrowers paid in this range, valued in USD at each hour's price, and how the pool split it between suppliers, the backstop and any protocol fee."
      source={asExample(`/v1/lending/pools/${pool}/revenue`, {
        interval: range.interval,
        window: range.window,
      })}
      bodyClassName="space-y-3"
    >
      <Segmented
        ariaLabel="Range"
        options={RANGES.map((r) => ({ label: r.label, value: r.id }))}
        value={rangeId}
        onChange={(v) => setRangeId(v as RangeOption['id'])}
      />
      {q.isLoading && (
        <p className="text-ink-muted text-sm">Loading revenue…</p>
      )}
      {q.isError && (
        <p role="alert" className="text-ink-muted text-sm">
          {isHistoryBuilding(q.error)
            ? 'Revenue history is still being built for this pool. Check back soon.'
            : 'Revenue is unavailable right now.'}
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
      {q.data && <RevenueBody d={q.data} hourly={range.interval === '1h'} />}
    </Panel>
  );
}
