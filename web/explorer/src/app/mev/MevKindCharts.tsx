'use client';

import { HBarList, StackedColumns } from '@/components/charts/Bars';
import { CATEGORICAL_PALETTE } from '@/components/charts/DonutChart';

const KIND_LABELS: Record<string, string> = {
  arbitrage: 'arbitrage',
  sandwich: 'sandwich',
  oracle_sandwich: 'oracle sandwich',
  liquidation_cascade: 'liq. cascade',
  wash_trade: 'wash trade',
};
const MAX_DAYS = 60;
const DAY_MS = 86_400_000;

export type MevKindEvent = { kind: string; detected_at: string };

/** Event counts per UTC day (oldest first, empty days kept) per kind. */
export function dailyByKind(events: readonly MevKindEvent[]) {
  const stamps = events
    .map((e) => ({ kind: e.kind, t: Date.parse(e.detected_at) }))
    .filter((e) => Number.isFinite(e.t));
  if (stamps.length === 0) return { days: [] as string[], byDay: new Map() };
  const day = (t: number) => Math.floor(t / DAY_MS);
  const last = Math.max(...stamps.map((s) => day(s.t)));
  const first = Math.max(
    Math.min(...stamps.map((s) => day(s.t))),
    last - MAX_DAYS + 1,
  );
  const days: string[] = [];
  for (let d = first; d <= last; d++)
    days.push(new Date(d * DAY_MS).toISOString().slice(0, 10));
  const byDay = new Map<string, Map<string, number>>();
  for (const s of stamps) {
    const key = new Date(day(s.t) * DAY_MS).toISOString().slice(0, 10);
    if (!days.includes(key)) continue;
    const m = byDay.get(key) ?? new Map<string, number>();
    m.set(s.kind, (m.get(s.kind) ?? 0) + 1);
    byDay.set(key, m);
  }
  return { days, byDay };
}

/**
 * MevKindCharts — by-kind counts and a daily stack for the events the feed
 * already fetched. Counts cover that fetched window only, not all history.
 */
export function MevKindCharts({ events }: { events: readonly MevKindEvent[] }) {
  if (events.length === 0) return null;
  const counts = new Map<string, number>();
  for (const e of events) counts.set(e.kind, (counts.get(e.kind) ?? 0) + 1);
  const kinds = [...counts.entries()].sort((a, b) => b[1] - a[1]);
  const color = (i: number) =>
    CATEGORICAL_PALETTE[i % (CATEGORICAL_PALETTE.length - 1)];
  const label = (k: string) => KIND_LABELS[k] ?? k;
  const { days, byDay } = dailyByKind(events);

  return (
    <div className="grid gap-6 md:grid-cols-2" data-testid="mev-kind-charts">
      <div className="space-y-2">
        <h3 className="text-ink-muted text-[11px] font-semibold tracking-wider uppercase">
          By kind
        </h3>
        <HBarList
          ariaLabel={`MEV events by kind, latest ${events.length} events`}
          items={kinds.map(([k, n], i) => ({
            id: k,
            label: label(k),
            value: n,
            color: color(i),
          }))}
        />
      </div>
      {days.length > 1 && (
        <div className="space-y-2">
          <h3 className="text-ink-muted text-[11px] font-semibold tracking-wider uppercase">
            Per day (UTC)
          </h3>
          <StackedColumns
            ariaLabel={`MEV events per UTC day by kind, ${days[0]} to ${days[days.length - 1]}`}
            series={kinds.map(([k], i) => ({
              label: label(k),
              color: color(i),
            }))}
            buckets={days.map((d) => ({
              label: d,
              values: kinds.map(([k]) => byDay.get(d)?.get(k) ?? 0),
            }))}
          />
        </div>
      )}
      <p className="text-ink-muted text-xs md:col-span-2">
        Latest {events.length} detected events only, not all history.
      </p>
    </div>
  );
}
