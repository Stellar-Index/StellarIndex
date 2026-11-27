'use client';

import { useState } from 'react';

import { Panel } from '@/components/reveal';
import { Button, Segmented, Skeleton, Stat } from '@/components/ui';
import { asExample } from '@/api/client';
import type { components } from '@/api/types';
import { fmtDateUTC } from '@/lib/account-format';

import { useLendingProtocols } from './LendingProtocolStats';
import { protocolRevenueReasonText } from './lendingRevenueText';
import { UsdFigure } from './lendingUsd';

type S = components['schemas'];
type Revenue = S['LendingProtocolRevenue'];
type Window = S['LendingProtocolRevenueWindow'];
type Exclusion = S['LendingProtocolRevenueExclusion'];

const WINDOWS = [
  { id: '1d', label: '1d' },
  { id: '7d', label: '7d' },
  { id: '30d', label: '30d' },
  { id: 'all_time', label: 'All time' },
] as const satisfies readonly { id: keyof Revenue; label: string }[];
type WindowId = (typeof WINDOWS)[number]['id'];

const PROTOCOLS = [
  { id: 'blend', name: 'Blend' },
  { id: 'blnt', name: 'BLNT V3' },
] as const;

/** What a window leaves out, counted by reason in plain words ("price withheld: 2 reserves"). */
export function exclusionSummary(excluded: readonly Exclusion[]): string[] {
  const by = new Map<string, Map<string, number>>();
  for (const e of excluded) {
    const r = protocolRevenueReasonText(e.reason);
    const noun = e.asset ? 'reserve' : e.pool ? 'pool' : 'protocol';
    if (!by.has(r)) by.set(r, new Map());
    const m = by.get(r)!;
    m.set(noun, (m.get(noun) ?? 0) + 1);
  }
  return [...by].map(
    ([r, nouns]) =>
      `${r}: ${[...nouns]
        .map(([noun, n]) =>
          noun === 'protocol'
            ? 'whole protocol'
            : `${n} ${noun}${n === 1 ? '' : 's'}`,
        )
        .join(', ')}`,
  );
}

function Leg({
  label,
  value,
  lowerBound,
  reason,
}: {
  label: string;
  value: string | null;
  lowerBound: boolean;
  reason: string | undefined;
}) {
  return (
    <div className="flex items-baseline justify-between gap-3">
      <dt className="text-ink-muted">{label}</dt>
      <dd className="tnum text-ink-body text-right">
        <UsdFigure
          value={value}
          lowerBound={lowerBound}
          reason={reason}
          showReason
        />
      </dd>
    </div>
  );
}

function Excluded({ w }: { w: Window }) {
  const lines = exclusionSummary(w.excluded);
  if (lines.length === 0) return null;
  return (
    <details className="text-ink-muted text-xs">
      <summary className="cursor-pointer">
        ≥ means at least: part of this window is left out
      </summary>
      <ul className="mt-1 list-disc pl-5" data-testid="revenue-excluded">
        {lines.map((l) => (
          <li key={l}>{l}</li>
        ))}
      </ul>
    </details>
  );
}

function ProtocolBlock({
  name,
  blnt,
  rev,
  win,
}: {
  name: string;
  blnt: boolean;
  rev: Revenue;
  win: WindowId;
}) {
  const w = rev[win];
  const degraded = rev.degraded && (
    <p className="text-ink-muted text-xs">Some values could not be read.</p>
  );
  let body;
  if (w.usd_reason === 'not_valued')
    body = (
      <p role="status" className="text-ink-muted text-sm">
        Revenue totals are not computed yet.
      </p>
    );
  else if (w.interest_accrued_usd == null)
    body = (
      <>
        <p role="status" className="text-ink-muted text-sm">
          Revenue cannot be shown for this window:{' '}
          {protocolRevenueReasonText(w.usd_reason)}.
        </p>
        <Excluded w={w} />
      </>
    );
  else {
    const lb = w.lower_bound;
    const why = (v: string | null) =>
      v == null ? protocolRevenueReasonText(w.usd_reason) : undefined;
    const v1 = w.versions?.v1;
    const v2 = w.versions?.v2;
    body = (
      <>
        <Stat
          label="Interest accrued"
          value={
            <UsdFigure
              value={w.interest_accrued_usd}
              lowerBound={lb}
              reason={lb ? 'part of this window is left out' : undefined}
            />
          }
          sub={
            w.through_day ? `through ${fmtDateUTC(w.through_day)}` : undefined
          }
        />
        <dl className="space-y-1 text-sm">
          <Leg
            label="Supplier share"
            value={w.supplier_share_usd}
            lowerBound={lb}
            reason={why(w.supplier_share_usd)}
          />
          <Leg
            label="Backstop take"
            value={w.backstop_take_usd}
            lowerBound={lb}
            reason={why(w.backstop_take_usd)}
          />
          {blnt && (
            <Leg
              label={
                rev.fee_rate_inferred
                  ? 'Protocol fee (inferred rate)'
                  : 'Protocol fee'
              }
              value={w.protocol_fee_usd}
              lowerBound={lb}
              reason={why(w.protocol_fee_usd)}
            />
          )}
          <Leg
            label="Paid out to backstop"
            value={w.backstop_drawn_usd}
            lowerBound
            reason={
              why(w.backstop_drawn_usd) ??
              'fills that state no lot are left out'
            }
          />
          <Leg
            label="Default losses"
            value={w.default_loss_usd}
            lowerBound={lb}
            reason={why(w.default_loss_usd)}
          />
        </dl>
        {(v1 || v2) && (
          <p className="text-ink-muted text-xs" data-testid="revenue-versions">
            Interest by pool version, each rounded on its own:{' '}
            {(
              [
                ['V1', v1],
                ['V2', v2],
              ] as const
            ).map(
              ([label, v], i) =>
                v && (
                  <span key={label}>
                    {i > 0 && v1 && ' · '}
                    {label}{' '}
                    <UsdFigure
                      value={v.interest_accrued_usd}
                      lowerBound={v.lower_bound}
                      reason={
                        v.interest_accrued_usd == null
                          ? protocolRevenueReasonText(v.usd_reason)
                          : undefined
                      }
                    />
                  </span>
                ),
            )}
          </p>
        )}
        {lb && <Excluded w={w} />}
      </>
    );
  }
  return (
    <section
      className="space-y-3"
      data-testid={`revenue-${blnt ? 'blnt' : 'blend'}`}
    >
      <h3 className="text-ink-body text-sm font-medium">{name}</h3>
      {body}
      {degraded}
    </section>
  );
}

/**
 * One protocol's interest accrued over a window, with how it was split and
 * what was paid out beside it. Each figure is the API's; nothing is summed.
 */
export function LendingProtocolRevenue({
  protocol,
}: {
  protocol: (typeof PROTOCOLS)[number]['id'];
}) {
  const q = useLendingProtocols();
  const [win, setWin] = useState<WindowId>('30d');
  if (q.error instanceof Error && q.error.message.startsWith('404'))
    return null;
  const shown = PROTOCOLS.filter((p) => p.id === protocol).flatMap((p) => {
    const rev = q.data?.find((r) => r.protocol === p.id)?.revenue;
    return rev ? [{ ...p, rev }] : [];
  });
  if (q.isSuccess && shown.length === 0) return null;
  const anyValued = shown.some((s) =>
    WINDOWS.some((w) => s.rev[w.id].usd_reason !== 'not_valued'),
  );
  return (
    <Panel
      headingLevel={2}
      title="Interest & revenue"
      hint="Interest borrowers owed across the protocol's pools, valued day by day in USD, how it was split, and what was paid out."
      source={asExample('/v1/lending/protocols', {})}
      bodyClassName="space-y-4"
    >
      {q.isLoading && <Skeleton className="h-16 w-full" />}
      {q.isError && (
        <p role="alert" className="text-ink-muted text-sm">
          Revenue is unavailable right now.
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
      {anyValued && (
        <Segmented
          ariaLabel="Window"
          options={WINDOWS.map((w) => ({ label: w.label, value: w.id }))}
          value={win}
          onChange={(v) => setWin(v as WindowId)}
        />
      )}
      {shown.map((s) => (
        <ProtocolBlock
          key={s.id}
          name={s.name}
          blnt={s.id === 'blnt'}
          rev={s.rev}
          win={win}
        />
      ))}
    </Panel>
  );
}
