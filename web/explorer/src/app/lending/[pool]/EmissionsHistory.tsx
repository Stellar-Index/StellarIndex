'use client';

import { useState } from 'react';
import { useQuery } from '@tanstack/react-query';

import { StackedColumns } from '@/components/charts/Bars';
import { Button, Segmented } from '@/components/ui';
import { apiGet, type Envelope } from '@/api/client';
import type { components, paths } from '@/api/types';
import {
  formatCompact,
  formatCompactTokenUnits,
  formatReadable,
  sumDecimalStrings,
  UNSCALED_LABEL,
} from '@/lib/format';
import { plotNumber } from '../chartColors';
import { RANGES, type RangeOption } from '../lendingInsights';
import { revenueReasonText } from '../lendingRevenueText';
import { useAssetText } from '../lendingUsd';

export type EmissionHistory =
  paths['/lending/pools/{pool}/emissions/history']['get']['responses'][200]['content']['application/json']['data'];
type Usd = components['schemas']['LendingEmissionUSD'];
type Reason =
  | NonNullable<Usd['usd_reason']>
  | EmissionHistory['excluded'][number]['reason']
  | NonNullable<EmissionHistory['reserves']['unavailable_reason']>
  | NonNullable<EmissionHistory['backstop_alloc']['unavailable_reason']>
  | NonNullable<EmissionHistory['backstop_claims']['unavailable_reason']>;

// Typed over the generated enums: a value the spec gains fails typecheck here.
const REASON_TEXT: Record<Reason, string> = {
  emission_token_unread: 'the pool’s emission token is not read',
  emission_token_unverified: 'the emission token is not the verified one',
  no_price_within_bound: 'no price close enough in time',
  amount_unread: 'some amounts could not be decoded',
  reads_disagree: 'the daily and hourly reads disagree',
  hourly_read_failed: 'the hourly read failed',
  unpriced: 'no USD market',
  decimals_unknown: 'token decimals unknown',
  price_scale_mismatch: 'price scale does not match the token',
  withheld: 'price withheld (flagged issuer or thin market)',
  price_read_failed: 'price could not be read',
  window_past_lake_tip: 'the range runs past the indexed ledgers',
  lake_tip_unknown: 'the indexed ledger tip could not be read',
  writer_behind_lake: 'still catching up with the indexed ledgers',
  writer_cursor_unread: 'indexing progress could not be read',
  history_unverified: 'history not yet verified back to the first ledger',
  history_verdict_stale: 'history verification is out of date',
  history_verdict_unread: 'history verification could not be read',
  claim_names_several_reserves: 'some claims name several reserves',
  claim_names_no_reserve: 'some claims name no reserve',
  v1_emission_schedule_unverified: 'Blend v1 schedules are not verified',
  v1_backstop_unaudited: 'Blend v1 backstops are not verified',
  backstop_claim_names_no_pool:
    'Blend backstop claims do not name a pool and pay LP tokens',
};

export function emissionReasonText(reason: string | null | undefined): string {
  return REASON_TEXT[reason as Reason] ?? revenueReasonText(reason);
}

const STREAMS = [
  { key: 'schedule', label: 'Scheduled', color: 'var(--color-chart-2)' },
  {
    key: 'pool',
    label: 'Claimed from the pool',
    color: 'var(--color-chart-4)',
  },
  {
    key: 'backstop',
    label: 'Claimed by backstop depositors',
    color: 'var(--color-chart-5)',
  },
] as const;
type StreamKey = (typeof STREAMS)[number]['key'];

export interface EmissionCell {
  /** Emission-token base units; null when the bucket is not served yet. */
  tokens: string | null;
  /** USD; null when nothing in it could be valued (see `reason`). */
  usd: string | null;
  partial: boolean;
  /** Some of the bucket's tokens have no USD value. */
  usdLowerBound: boolean;
  reason: string | null;
}

export interface EmissionStream {
  key: StreamKey;
  label: string;
  color: string;
  available: boolean;
  unavailableReason: string | null;
  /** Every figure of the stream is a lower bound, with the reasons. */
  lowerBound: string[];
  cells: EmissionCell[];
  totalTokens: string | null;
  totalUsd: string | null;
  totalUsdLowerBound: boolean;
}

type Pt = { t: string; partial: boolean; amount: string; usd: Usd };

const isZero = (v: string) => /^-?0+(\.0+)?$/.test(v);

/** Bucket starts over [from, to) at the response's interval. */
export function bucketStarts(d: EmissionHistory): number[] {
  const step = d.interval === '1h' ? 3_600_000 : 86_400_000;
  const from = Date.parse(d.from);
  const to = Date.parse(d.to);
  const out: number[] = [];
  if (!Number.isFinite(from) || !Number.isFinite(to)) return out;
  for (let t = from; t < to && out.length < 2000; t += step) out.push(t);
  return out;
}

function cells(
  starts: number[],
  servedThrough: string | null,
  lines: Pt[][],
): EmissionCell[] {
  const end = servedThrough == null ? -Infinity : Date.parse(servedThrough);
  const by = new Map<number, Pt[]>();
  for (const line of lines)
    for (const p of line) {
      const k = Date.parse(p.t);
      by.set(k, [...(by.get(k) ?? []), p]);
    }
  return starts.map((t) => {
    if (!(t < end))
      return {
        tokens: null,
        usd: null,
        partial: false,
        usdLowerBound: false,
        reason: 'not indexed yet',
      };
    // A bucket with no point is served and empty: zero tokens, zero USD.
    const ps = by.get(t) ?? [];
    const valued = ps.filter((p) => p.usd.amount_usd != null);
    const unvalued = ps.filter(
      (p) => p.usd.amount_usd == null && !isZero(p.amount),
    );
    return {
      tokens: sumDecimalStrings(ps.map((p) => p.amount)) ?? '0',
      usd:
        valued.length === 0 && unvalued.length > 0
          ? null
          : (sumDecimalStrings(valued.map((p) => p.usd.amount_usd)) ?? '0'),
      partial: ps.some((p) => p.partial),
      usdLowerBound:
        valued.length > 0 &&
        (unvalued.length > 0 || valued.some((p) => p.usd.lower_bound)),
      reason: unvalued[0]?.usd.usd_reason ?? null,
    };
  });
}

/** The served streams as chart rows; the API never adds them together, so neither does this. */
export function emissionStreams(d: EmissionHistory): EmissionStream[] {
  const starts = bucketStarts(d);
  const lb = (stream: EmissionHistory['excluded'][number]['stream']) =>
    d.excluded
      .filter((e) => e.stream === stream || e.stream === 'window')
      .map((e) => emissionReasonText(e.reason));
  const r = d.reserves;
  const pc = d.pool_claims;
  const bc = d.backstop_claims;
  const schedLines: Pt[][] = r.streams.map((s) =>
    s.points.map((p) => ({ ...p, amount: p.tokens })),
  );
  const bcLines: Pt[][] = bc.tiers.map((t) =>
    t.points.map((p) => ({ ...p, amount: p.blnt_amount })),
  );
  const total = (usds: Usd[]) => ({
    totalUsd: usds.some((u) => u.amount_usd == null)
      ? null
      : sumDecimalStrings(usds.map((u) => u.amount_usd)),
    totalUsdLowerBound: usds.some((u) => u.lower_bound),
  });
  const out: EmissionStream[] = [
    {
      ...STREAMS[0],
      available: r.available,
      unavailableReason: r.unavailable_reason,
      lowerBound: r.lower_bound ? lb('reserves') : [],
      cells: cells(starts, r.served_through, schedLines),
      totalTokens: sumDecimalStrings(r.streams.map((s) => s.tokens)) ?? '0',
      ...total(r.streams.map((s) => s.usd)),
    },
    {
      ...STREAMS[1],
      available: pc.available,
      unavailableReason: pc.unavailable_reason,
      lowerBound: pc.lower_bound ? lb('pool_claims') : [],
      cells: cells(starts, pc.served_through, [pc.points]),
      totalTokens: pc.totals.amount,
      ...total([pc.usd]),
    },
    {
      ...STREAMS[2],
      available: bc.available,
      unavailableReason: bc.unavailable_reason,
      lowerBound: bc.lower_bound ? lb('backstop_claims') : [],
      cells: cells(starts, bc.served_through, bcLines),
      totalTokens:
        sumDecimalStrings(bc.tiers.map((t) => t.totals.blnt_amount)) ?? '0',
      ...total(bc.tiers.map((t) => t.usd)),
    },
  ];
  return out;
}

export type Unit = 'usd' | 'tokens';

/** A stream's total in the chosen unit, "at least" unless every bucket it covers is exact. */
function totalText(
  s: EmissionStream,
  unit: Unit,
  tokenText: (raw: string) => string,
): string {
  const unserved = s.cells.some((c) => c.tokens == null);
  if (unit === 'usd' && s.totalUsd != null) {
    const exact =
      !unserved &&
      s.lowerBound.length === 0 &&
      !s.totalUsdLowerBound &&
      s.cells.every((c) => c.usd != null && !c.usdLowerBound);
    return `${exact ? '' : 'at least '}${formatReadable(s.totalUsd, true) ?? '—'}`;
  }
  const exact = !unserved && s.lowerBound.length === 0;
  const tokens = `${exact ? '' : 'at least '}${tokenText(s.totalTokens ?? '0')}`;
  // USD asked for but none served: say so rather than switch units silently.
  return unit === 'usd' ? `${tokens} (not priced)` : tokens;
}

/** One sentence over the streams shown: what was scheduled and what was claimed. */
export function insightText(
  streams: EmissionStream[],
  unit: Unit,
  rangeLabel: string,
  tokenText: (raw: string) => string,
): string | null {
  const shown = streams.filter((s) => s.available);
  if (shown.length === 0) return null;
  const parts = shown.map((s) => {
    const v = totalText(s, unit, tokenText);
    switch (s.key) {
      case 'schedule':
        return `${v} scheduled to the reserves`;
      case 'pool':
        return `${v} claimed from the pool`;
      case 'backstop':
        return `${v} claimed by backstop depositors`;
    }
  });
  return `Over ${rangeLabel}: ${parts.join('; ')}.`;
}

function cellNote(c: EmissionCell, unit: Unit): string | undefined {
  const notes: string[] = [];
  if (c.tokens == null) notes.push('not indexed yet');
  else if (unit === 'usd' && c.usd == null)
    notes.push(`no USD value: ${emissionReasonText(c.reason)}`);
  else if (unit === 'usd' && c.usdLowerBound)
    notes.push('some hours had no USD price');
  if (c.partial) notes.push('bucket cut short by the range end');
  return notes.length ? notes.join('; ') : undefined;
}

function StreamChart({
  s,
  unit,
  starts,
  hourly,
  max,
  scale,
  formatValue,
}: {
  s: EmissionStream;
  unit: Unit;
  starts: number[];
  hourly: boolean;
  max: number;
  scale: (raw: string | null) => number | null;
  formatValue: (n: number) => string;
}) {
  const zero = s.cells.every((c) => c.tokens != null && isZero(c.tokens));
  return (
    <div className="space-y-1" data-testid={`emissions-stream-${s.key}`}>
      <h3 className="text-ink-body text-xs font-medium">{s.label}</h3>
      {zero ? (
        <p className="text-ink-muted text-xs">None in this range.</p>
      ) : (
        <StackedColumns
          buckets={s.cells.map((c, i) => {
            const v = scale(unit === 'usd' ? c.usd : c.tokens);
            const iso = new Date(starts[i]).toISOString();
            return {
              label: hourly
                ? `${iso.slice(0, 10)} ${iso.slice(11, 16)}`
                : iso.slice(0, 10),
              values: [v ?? 0],
              missing: v == null,
              hatched: unit === 'usd' && c.usdLowerBound,
              note: cellNote(c, unit),
            };
          })}
          series={[{ label: s.label, color: s.color }]}
          formatValue={formatValue}
          height={96}
          max={max}
          ariaLabel={`${s.label} per ${hourly ? 'hour' : 'day'}, ${s.cells.length} buckets`}
        />
      )}
      {s.lowerBound.length > 0 && (
        <p className="text-ink-muted text-xs">
          Every figure here is a lower bound: {s.lowerBound.join('; ')}.
        </p>
      )}
    </div>
  );
}

function HistoryBody({
  d,
  range,
  unit,
  onUnit,
}: {
  d: EmissionHistory;
  range: RangeOption;
  unit: Unit | null;
  onUnit: (u: Unit) => void;
}) {
  const assetText = useAssetText();
  const token = d.emission_token ? assetText(d.emission_token) : 'tokens';
  const dec = d.emission_token_decimals;
  const streams = emissionStreams(d);
  const shown = streams.filter((s) => s.available);
  const off = streams.filter((s) => !s.available);
  const anyUsd = shown.some((s) => s.cells.some((c) => c.usd != null));
  const u: Unit = unit ?? (anyUsd ? 'usd' : 'tokens');
  const tokenText = (raw: string) =>
    `${formatCompactTokenUnits(raw, dec)} ${token}`;
  // Geometry only; every figure printed as text comes from the decimal strings.
  const scale = (raw: string | null) => {
    const n = plotNumber(raw);
    return n == null || u === 'usd' || dec == null ? n : n / 10 ** dec;
  };
  const unitText = dec == null ? `${token} ${UNSCALED_LABEL}` : token;
  const max = Math.max(
    0,
    ...shown.flatMap((s) =>
      s.cells.map((c) => scale(u === 'usd' ? c.usd : c.tokens) ?? 0),
    ),
  );
  const hourly = range.interval === '1h';
  const starts = bucketStarts(d);
  const empty =
    shown.length > 0 &&
    shown.every((s) =>
      s.cells.every((c) => c.tokens != null && isZero(c.tokens)),
    );
  const insight = insightText(
    streams,
    u,
    range.label.replace(' (hourly)', ''),
    tokenText,
  );

  return (
    <>
      {shown.length === 0 ? (
        <p role="status" className="text-ink-muted text-sm">
          Emission history cannot be shown:{' '}
          {off.map((s) => emissionReasonText(s.unavailableReason)).join('; ')}.
        </p>
      ) : empty ? (
        <p role="status" className="text-ink-muted text-sm">
          No emissions in this range.
        </p>
      ) : (
        <>
          {insight && (
            <p
              className="text-ink-body text-sm"
              data-testid="emissions-insight"
            >
              {insight}
            </p>
          )}
          <Segmented
            ariaLabel="Unit"
            options={[
              { label: 'USD', value: 'usd' },
              { label: token, value: 'tokens' },
            ]}
            value={u}
            onChange={(v) => onUnit(v as Unit)}
          />
          {shown.map((s) => (
            <StreamChart
              key={s.key}
              s={s}
              unit={u}
              starts={starts}
              hourly={hourly}
              max={max}
              scale={scale}
              formatValue={(n) =>
                u === 'usd'
                  ? `$${formatCompact(n)}`
                  : `${formatCompact(n)} ${unitText}`
              }
            />
          ))}
          <p className="text-ink-muted text-xs">
            The streams are the same tokens at different stages, so they are
            charted apart and never added. Hatched columns are lower bounds;
            faint bands have no value.
          </p>
        </>
      )}
      {shown.length > 0 && off.length > 0 && (
        <p className="text-ink-muted text-xs">
          {off
            .map(
              (s) => `${s.label}: ${emissionReasonText(s.unavailableReason)}`,
            )
            .join('. ')}
          .
        </p>
      )}
    </>
  );
}

/** Emissions per bucket by stream, on the pool page's emissions panel. */
export function EmissionsHistory({ pool }: { pool: string }) {
  const [rangeId, setRangeId] = useState<RangeOption['id']>('90d');
  const [unit, setUnit] = useState<Unit | null>(null);
  const range = RANGES.find((r) => r.id === rangeId) ?? RANGES[1];
  const q = useQuery({
    queryKey: ['/v1/lending/pools/{pool}/emissions/history', pool, range.id],
    retry: false,
    staleTime: 300_000,
    queryFn: async () =>
      (
        await apiGet<Envelope<EmissionHistory>>(
          `/v1/lending/pools/${encodeURIComponent(pool)}/emissions/history`,
          { interval: range.interval, window: range.window },
        )
      ).data,
  });
  if (q.error instanceof Error && q.error.message.startsWith('404'))
    return null;
  return (
    <section className="space-y-3" aria-label="Emissions over time">
      <Segmented
        ariaLabel="Range"
        options={RANGES.map((r) => ({ label: r.label, value: r.id }))}
        value={rangeId}
        onChange={(v) => setRangeId(v as RangeOption['id'])}
      />
      {q.isLoading && (
        <p className="text-ink-muted text-sm">Loading emission history…</p>
      )}
      {q.isError && (
        <p role="alert" className="text-ink-muted text-sm">
          Emission history is unavailable right now.
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
      {q.data && (
        <HistoryBody d={q.data} range={range} unit={unit} onUnit={setUnit} />
      )}
    </section>
  );
}
