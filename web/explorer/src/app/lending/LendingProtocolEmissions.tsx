'use client';

import { useState } from 'react';

import { Panel } from '@/components/reveal';
import { Button, Segmented, Skeleton } from '@/components/ui';
import { asExample } from '@/api/client';
import type { components } from '@/api/types';
import { fmtDateUTC } from '@/lib/account-format';
import { formatCompactTokenUnits } from '@/lib/format';

import { useLendingProtocols } from './LendingProtocolStats';
import {
  REVENUE_REASON_TEXT,
  protocolRevenueReasonText,
} from './lendingRevenueText';
import { UsdFigure, useAssetText } from './lendingUsd';

type S = components['schemas'];
type Emissions = S['LendingProtocolEmissions'];
type Window = S['LendingProtocolEmissionsWindow'];
type Amounts = S['LendingProtocolEmissionAmounts'];
type Exclusion = S['LendingProtocolEmissionsExclusion'];
type StreamId = keyof Window['streams'];

const WINDOWS = [
  { id: '1d', label: '1d' },
  { id: '7d', label: '7d' },
  { id: '30d', label: '30d' },
  { id: 'all_time', label: 'All time' },
] as const satisfies readonly { id: keyof Emissions; label: string }[];
type WindowId = (typeof WINDOWS)[number]['id'];
type Unit = 'usd' | 'tokens';

// One row per stream; they are different layers of the same tokens and are never added.
const STREAMS = [
  {
    id: 'reserve_schedule',
    label: 'Scheduled to reserves',
    about: 'Tokens the protocol’s schedule sets aside for each reserve.',
  },
  {
    id: 'backstop_alloc_pool',
    label: 'Allocated to pools',
    about: 'Tokens the backstop’s emission cycle passed on to pools.',
  },
  {
    id: 'backstop_alloc_backstop',
    label: 'Allocated to backstop depositors',
    about: 'Tokens the backstop’s emission cycle kept for its own depositors.',
  },
  {
    id: 'pool_claims',
    label: 'Claimed from pools',
    about: 'Tokens users actually claimed from pool reserves.',
  },
  {
    id: 'backstop_claims',
    label: 'Claimed by backstop depositors',
    about: 'Tokens backstop depositors actually claimed.',
  },
] as const satisfies readonly { id: StreamId; label: string; about: string }[];

const PROTOCOLS = [
  { id: 'blend', name: 'Blend' },
  { id: 'blnt', name: 'BLNT V3' },
] as const;

// Reasons only the emissions block emits; the rest read as on the revenue block.
const EMISSIONS_REASON_TEXT: Record<string, string> = {
  stream_unavailable: 'no valued pool serves this stream',
  emission_token_mixed:
    'pools emit more than one token, so no token total is given',
  emission_token_not_stored:
    'the emission token is not recorded on the stored rows',
  emission_token_decimals_unknown:
    'the emission token’s decimals are not recorded',
  emissions_read_failed: 'stored emissions could not be read',
  emissions_timed_out: 'reading emissions took too long',
  history_unverified: 'this stream’s history is not verified complete yet',
  history_verdict_stale: 'the history check for this stream is out of date',
  history_verdict_unread: 'the history check could not be read',
};

/** Plain words for an emissions reason; an unknown code is shown as the API sent it. */
export function emissionsReasonText(reason: string | null | undefined): string {
  if (!reason) return 'no value';
  if (EMISSIONS_REASON_TEXT[reason]) return EMISSIONS_REASON_TEXT[reason];
  return reason in REVENUE_REASON_TEXT
    ? protocolRevenueReasonText(reason)
    : reason;
}

/** What a window leaves out, counted by reason in plain words. */
export function emissionsExclusionSummary(
  excluded: readonly Exclusion[],
): string[] {
  const by = new Map<string, Map<string, number>>();
  for (const e of excluded) {
    const r = emissionsReasonText(e.reason);
    const noun = e.pool ? 'pool' : 'whole protocol';
    if (!by.has(r)) by.set(r, new Map());
    const m = by.get(r)!;
    m.set(noun, (m.get(noun) ?? 0) + 1);
  }
  return [...by].map(
    ([r, nouns]) =>
      `${r}: ${[...nouns]
        .map(([noun, n]) =>
          noun === 'whole protocol'
            ? noun
            : `${n} ${noun}${n === 1 ? '' : 's'}`,
        )
        .join(', ')}`,
  );
}

function TokenFigure({
  value,
  decimals,
  token,
  lowerBound,
  reason,
}: {
  value: string | null;
  decimals: number | null;
  token: string;
  lowerBound: boolean;
  reason: string | undefined;
}) {
  const shown = value == null ? null : formatCompactTokenUnits(value, decimals);
  if (shown == null || shown === '—')
    return (
      <span className="text-ink-faint" title={reason ?? 'No token amount'}>
        —
        {reason && (
          <span className="text-ink-muted block text-xs font-normal">
            {reason}
          </span>
        )}
      </span>
    );
  return (
    <span
      title={`${lowerBound ? 'At least ' : ''}${shown} ${token}${reason ? `; ${reason}` : ''}`}
    >
      {lowerBound && (
        <>
          <span aria-hidden="true">≥</span>
          <span className="sr-only">at least </span>
        </>
      )}
      {shown} {token}
    </span>
  );
}

function Row({
  label,
  about,
  a,
  unit,
  decimals,
  token,
}: {
  label: string;
  about: string;
  a: Amounts;
  unit: Unit;
  decimals: number | null;
  token: string;
}) {
  const usd = unit === 'usd';
  const lb = usd ? a.lower_bound : a.amount_lower_bound;
  const reasonCode = usd ? a.usd_reason : a.amount_reason;
  const why = (v: string | null) =>
    v == null ? emissionsReasonText(reasonCode) : undefined;
  return (
    <div className="flex items-baseline justify-between gap-3">
      <dt>
        <span className="text-ink-body">{label}</span>
        <span className="text-ink-muted block text-xs">{about}</span>
      </dt>
      <dd className="tnum text-ink-body text-right">
        {usd ? (
          <UsdFigure
            value={a.amount_usd}
            lowerBound={lb}
            reason={
              why(a.amount_usd) ??
              (lb ? 'part of this window is left out' : undefined)
            }
            showReason
          />
        ) : (
          <TokenFigure
            value={a.amount}
            decimals={decimals}
            token={token}
            lowerBound={lb}
            reason={
              why(a.amount) ??
              (lb ? 'part of this window is left out' : undefined)
            }
          />
        )}
        {a.approximate && (
          <span
            tabIndex={0}
            role="note"
            aria-label="approximate: part of this figure is rebuilt from events and is accurate to dust, not exact"
            title="Part of this figure is rebuilt from events and is accurate to dust, not exact."
            className="text-ink-muted ml-1 cursor-help"
          >
            ≈
          </span>
        )}
      </dd>
    </div>
  );
}

function Block({
  name,
  data,
  win,
  unit,
  onUnit,
}: {
  name: string;
  data: Emissions;
  win: WindowId;
  unit: Unit | null;
  onUnit: (u: Unit) => void;
}) {
  const assetText = useAssetText();
  const w = data[win];
  const resolved = data.emission_token ? assetText(data.emission_token) : null;
  // Never a bare address as the label.
  const token =
    resolved && !resolved.includes('…') ? resolved : 'emission token';
  const decimals = data.emission_token_decimals;
  const all = STREAMS.map((s) => ({ ...s, a: w.streams[s.id] }));
  const notComputed = all.every((s) => s.a.usd_reason === 'not_valued');
  const anyUsd = all.some((s) => s.a.amount_usd != null);
  const u: Unit = unit ?? (anyUsd ? 'usd' : 'tokens');
  const lines = emissionsExclusionSummary(w.excluded);
  let body;
  if (data.degraded)
    body = (
      <p role="status" className="text-ink-muted text-sm">
        Emissions figures are temporarily unavailable.
      </p>
    );
  else if (notComputed)
    body = (
      <p role="status" className="text-ink-muted text-sm">
        Emissions totals are not computed yet.
      </p>
    );
  else
    body = (
      <>
        <Segmented
          ariaLabel="Unit"
          options={[
            { label: 'USD', value: 'usd' },
            { label: token, value: 'tokens' },
          ]}
          value={u}
          onChange={(v) => onUnit(v as Unit)}
        />
        {w.through_day && (
          <p className="text-ink-muted text-xs">
            through {fmtDateUTC(w.through_day)}
          </p>
        )}
        <dl className="space-y-2 text-sm">
          {all.map((s) => (
            <Row
              key={s.id}
              label={s.label}
              about={s.about}
              a={s.a}
              unit={u}
              decimals={decimals}
              token={token}
            />
          ))}
        </dl>
        <p className="text-ink-muted text-xs">
          Each row is its own stream of the same tokens; they are not added
          together.
        </p>
        {all.some((s) => s.a.approximate) && (
          <p className="text-ink-muted text-xs" data-testid="emissions-approx">
            ≈ Part of this figure is rebuilt from events and is accurate to
            dust, not exact.
          </p>
        )}
        {lines.length > 0 && (
          <details className="text-ink-muted text-xs">
            <summary className="cursor-pointer">
              ≥ means at least: part of this window is left out
            </summary>
            <ul
              className="mt-1 list-disc pl-5"
              data-testid="emissions-excluded"
            >
              {lines.map((l) => (
                <li key={l}>{l}</li>
              ))}
            </ul>
          </details>
        )}
      </>
    );
  return (
    <section
      className="space-y-3"
      data-testid={`emissions-${name === 'Blend' ? 'blend' : 'blnt'}`}
    >
      <h3 className="text-ink-body text-sm font-medium">{name}</h3>
      {body}
    </section>
  );
}

/**
 * One protocol's token emissions over a window, a row per stream. Each figure
 * is the API's; streams are never summed and null is never shown as zero.
 */
export function LendingProtocolEmissions({
  protocol,
}: {
  protocol: (typeof PROTOCOLS)[number]['id'];
}) {
  const q = useLendingProtocols();
  const [win, setWin] = useState<WindowId>('30d');
  const [unit, setUnit] = useState<Unit | null>(null);
  if (q.error instanceof Error && q.error.message.startsWith('404'))
    return null;
  const p = PROTOCOLS.find((x) => x.id === protocol)!;
  const data = q.data?.find((r) => r.protocol === protocol)?.emissions;
  if (q.isSuccess && !data) return null;
  return (
    <Panel
      headingLevel={2}
      title="Token emissions"
      hint="Reward tokens the protocol schedules, allocates and has paid out, by stream. Streams are separate layers of the same tokens and are never added together."
      source={asExample('/v1/lending/protocols', {})}
      bodyClassName="space-y-4"
    >
      {q.isLoading && <Skeleton className="h-16 w-full" />}
      {q.isError && (
        <p role="alert" className="text-ink-muted text-sm">
          Emissions are unavailable right now.
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
      {data && (
        <>
          <Segmented
            ariaLabel="Window"
            options={WINDOWS.map((w) => ({ label: w.label, value: w.id }))}
            value={win}
            onChange={(v) => setWin(v as WindowId)}
          />
          <Block
            name={p.name}
            data={data}
            win={win}
            unit={unit}
            onUnit={setUnit}
          />
        </>
      )}
    </Panel>
  );
}
