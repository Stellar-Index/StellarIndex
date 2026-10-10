'use client';

import { useState } from 'react';

import { HBarList, type HBarItem } from '@/components/charts/Bars';
import { CATEGORICAL_PALETTE } from '@/components/charts/DonutChart';
import { formatCompact } from '@/lib/format';

import { formatTimestamp, scaledUnits } from '../explorer-shared';

export interface CodeVersion {
  ledger?: number;
  close_time?: string;
  wasm_hash?: string;
}

export interface CodeSegment {
  index: number;
  ledger: number;
  wasmHash: string;
  start: string;
  /** ISO end, or null while this version is the one running now. */
  end: string | null;
  /** Share of the start-to-now span, 0..1. Geometry only. */
  share: number;
}

/**
 * codeTimelineSegments — each WASM version's time in service, from its
 * close_time to the next version's (the last runs to `now`). Returns []
 * when any version lacks a parseable time, so no span is guessed.
 */
export function codeTimelineSegments(
  versions: readonly CodeVersion[],
  now: number,
): CodeSegment[] {
  const times = versions.map((v) => Date.parse(v.close_time ?? ''));
  if (times.length === 0 || times.some((t) => !Number.isFinite(t))) return [];
  for (let i = 1; i < times.length; i++) {
    if (times[i]! < times[i - 1]!) return [];
  }
  const first = times[0]!;
  const span = Math.max(now, times[times.length - 1]!) - first;
  return versions.map((v, i) => {
    const start = times[i]!;
    const endMs = i + 1 < times.length ? times[i + 1]! : Math.max(now, start);
    return {
      index: i + 1,
      ledger: v.ledger ?? 0,
      wasmHash: v.wasm_hash ?? '',
      start: v.close_time!,
      end: i + 1 < versions.length ? versions[i + 1]!.close_time! : null,
      share: span > 0 ? (endMs - start) / span : 1 / versions.length,
    };
  });
}

/** CodeTimeline — the upgrade history as one bar, a segment per version. */
export function CodeTimeline({
  versions,
  now,
}: {
  versions: readonly CodeVersion[];
  now?: number;
}) {
  const [mountedAt] = useState(() => Date.now());
  const segs = codeTimelineSegments(versions, now ?? mountedAt);
  if (segs.length < 2) return null;
  return (
    <div className="space-y-1.5 px-4 pb-3" data-testid="code-timeline">
      <div
        role="img"
        aria-label={`${segs.length} WASM versions over time`}
        className="flex h-3 w-full overflow-hidden rounded-xs"
      >
        {segs.map((s) => (
          <span
            key={`${s.ledger}-${s.wasmHash}`}
            className="h-full min-w-[3px] border-r border-[var(--color-surface)] last:border-r-0"
            style={{
              width: `${s.share * 100}%`,
              backgroundColor:
                CATEGORICAL_PALETTE[
                  (s.index - 1) % (CATEGORICAL_PALETTE.length - 1)
                ],
            }}
            title={`v${s.index} ${s.wasmHash.slice(0, 12)}… from ${formatTimestamp(s.start)}${s.end ? ` to ${formatTimestamp(s.end)}` : ', running now'}`}
          />
        ))}
      </div>
      <div className="text-ink-faint flex justify-between font-mono text-[10px]">
        <span>{segs[0]!.start.slice(0, 10)}</span>
        <span>now</span>
      </div>
    </div>
  );
}

export interface FlowRow {
  event_kind?: string;
  from?: string;
  to?: string;
  amount?: string;
}

export interface FlowParty {
  address: string;
  /** Exact base-unit sum. */
  raw: bigint;
  count: number;
}

export interface FlowSummary {
  rows: number;
  kinds: { kind: string; count: number }[];
  senders: FlowParty[];
  receivers: FlowParty[];
}

const INT_RE = /^-?\d+$/;

/**
 * tokenFlowSummary — event mix and the largest senders and receivers over
 * the rows given. Only `transfer` rows move tokens; an approve amount is
 * an allowance, so it is counted in the mix but never summed.
 */
export function tokenFlowSummary(
  rows: readonly FlowRow[],
  top = 5,
): FlowSummary {
  const kinds = new Map<string, number>();
  const send = new Map<string, FlowParty>();
  const recv = new Map<string, FlowParty>();
  const add = (m: Map<string, FlowParty>, addr: string, amt: bigint) => {
    const p = m.get(addr) ?? { address: addr, raw: 0n, count: 0 };
    p.raw += amt;
    p.count += 1;
    m.set(addr, p);
  };
  for (const r of rows) {
    const kind = r.event_kind || 'unknown';
    kinds.set(kind, (kinds.get(kind) ?? 0) + 1);
    if (kind !== 'transfer' || !r.amount || !INT_RE.test(r.amount)) continue;
    const amt = BigInt(r.amount);
    if (r.from) add(send, r.from, amt);
    if (r.to) add(recv, r.to, amt);
  }
  const rank = (m: Map<string, FlowParty>) =>
    [...m.values()]
      .sort((a, b) => (b.raw > a.raw ? 1 : b.raw < a.raw ? -1 : 0))
      .slice(0, top);
  return {
    rows: rows.length,
    kinds: [...kinds.entries()]
      .map(([kind, count]) => ({ kind, count }))
      .sort((a, b) => b.count - a.count || a.kind.localeCompare(b.kind)),
    senders: rank(send),
    receivers: rank(recv),
  };
}

const short = (a: string) => `${a.slice(0, 6)}…${a.slice(-4)}`;

/** TokenFlowSummary — the mix and top parties of the rows the table lists. */
export function TokenFlowSummary({
  rows,
  decimals,
  formatAmount,
}: {
  rows: readonly FlowRow[];
  decimals: number;
  formatAmount: (raw: string) => string;
}) {
  const s = tokenFlowSummary(rows);
  if (s.rows === 0) return null;
  const parties = (ps: FlowParty[]): HBarItem[] =>
    ps.map((p) => ({
      id: p.address,
      label: short(p.address),
      title: p.address,
      value: scaledUnits(p.raw.toString(), decimals),
      display: formatAmount(p.raw.toString()),
      annotation: `${p.count} transfer${p.count === 1 ? '' : 's'}`,
    }));
  return (
    <div className="space-y-3 px-4 pb-3" data-testid="token-flow-summary">
      <div className="grid gap-4 md:grid-cols-3">
        <div className="space-y-1.5">
          <h3 className="text-ink-muted text-[11px] font-semibold tracking-wider uppercase">
            Event mix
          </h3>
          <HBarList
            ariaLabel="Audit-trail rows by event kind"
            formatValue={formatCompact}
            items={s.kinds.map((k, i) => ({
              id: k.kind,
              label: k.kind,
              value: k.count,
              color: CATEGORICAL_PALETTE[i % CATEGORICAL_PALETTE.length],
            }))}
          />
        </div>
        {s.senders.length > 0 && (
          <div className="space-y-1.5">
            <h3 className="text-ink-muted text-[11px] font-semibold tracking-wider uppercase">
              Top senders
            </h3>
            <HBarList
              ariaLabel="Top senders by amount"
              items={parties(s.senders)}
            />
          </div>
        )}
        {s.receivers.length > 0 && (
          <div className="space-y-1.5">
            <h3 className="text-ink-muted text-[11px] font-semibold tracking-wider uppercase">
              Top receivers
            </h3>
            <HBarList
              ariaLabel="Top receivers by amount"
              items={parties(s.receivers)}
            />
          </div>
        )}
      </div>
      <p className="text-ink-faint text-[11px]">
        Over the {s.rows} newest audit-trail rows only, not all-time. Mint, burn
        and clawback are in supply analytics, not here.
      </p>
    </div>
  );
}
