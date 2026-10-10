'use client';

import { Badge } from '@/components/ui';
import { HBarList, StackedColumns } from '@/components/charts/Bars';
import {
  CATEGORICAL_PALETTE,
  DonutChart,
} from '@/components/charts/DonutChart';
import { formatCompact } from '@/lib/format';

export const UNATTRIBUTED = 'Unattributed';

/** Share bar for a table cell; the number stays beside it. */
export function InlineBar({
  value,
  max,
  label,
}: {
  value: number;
  max: number;
  label: string;
}) {
  const pct = max > 0 && value > 0 ? Math.max((value / max) * 100, 2) : 0;
  return (
    <span
      role="img"
      aria-label={label}
      title={label}
      className="bg-surface-muted inline-block h-1.5 w-20 overflow-hidden rounded-xs align-middle"
    >
      <span
        className="bg-brand-500 block h-full"
        style={{ width: `${pct}%` }}
      />
    </span>
  );
}

/** Sums `value` per protocol; rows with no protocol tag share one Unattributed slice. */
export function protocolSlices<T extends { protocol?: string | null }>(
  rows: T[],
  value: (r: T) => number,
): { label: string; value: number; color?: string }[] {
  const byKey = new Map<string, number>();
  for (const r of rows) {
    const k = r.protocol || UNATTRIBUTED;
    byKey.set(k, (byKey.get(k) ?? 0) + value(r));
  }
  return [...byKey].map(([label, v]) => ({
    label,
    value: v,
    ...(label === UNATTRIBUTED ? { color: CATEGORICAL_PALETTE.at(-1) } : {}),
  }));
}

/** Events per protocol; contracts with no protocol tag get their own slice. */
export function ProtocolMixDonut({
  rows,
}: {
  rows: { protocol?: string | null; events?: number | null }[];
}) {
  const data = protocolSlices(rows, (r) => r.events ?? 0);
  if (data.every((d) => d.value <= 0)) return null;
  return (
    <DonutChart
      data={data}
      size={140}
      thickness={18}
      centerLabel={formatCompact(data.reduce((s, d) => s + d.value, 0))}
      centerSub="top 100 · 30d"
      formatValue={formatCompact}
    />
  );
}

/** Active ledgers per day over the activity window. */
export function ContractDailyBars({
  daily,
}: {
  daily: { date?: string; active_ledgers?: number }[];
}) {
  return (
    <StackedColumns
      ariaLabel="Active ledgers per day, last 30 days"
      height={80}
      series={[
        { label: 'Active ledgers / day', color: 'var(--color-brand-500)' },
      ]}
      buckets={daily.map((d) => ({
        label: d.date?.slice(0, 10) ?? '',
        values: [d.active_ledgers ?? 0],
      }))}
    />
  );
}

/** Mix of the loaded events by topic_0. Covers the loaded page only. */
export function EventMixDonut({
  events,
}: {
  events: { topic_0?: string; event_type?: string }[];
}) {
  const counts = new Map<string, number>();
  for (const e of events) {
    const k = e.topic_0 || e.event_type || 'unknown';
    counts.set(k, (counts.get(k) ?? 0) + 1);
  }
  if (counts.size === 0) return null;
  return (
    <div className="space-y-1">
      <DonutChart
        data={[...counts].map(([label, value]) => ({ label, value }))}
        size={120}
        thickness={16}
        centerLabel={String(events.length)}
        centerSub="loaded events"
      />
      <p className="text-ink-faint text-[11px]">
        Partial: only the {events.length} events loaded below.
      </p>
    </div>
  );
}

type RegistryRow = { name: string; category: string; contract_count: number };

/** Registered contracts by category (donut) and by protocol (ranked bars). */
export function RegistryCharts({ rows }: { rows: RegistryRow[] }) {
  const cats = new Map<string, number>();
  for (const r of rows) {
    const k = r.category || 'uncategorised';
    cats.set(k, (cats.get(k) ?? 0) + r.contract_count);
  }
  const ranked = rows.filter((r) => r.contract_count > 0).slice(0, 10);
  if (ranked.length === 0) return null;
  return (
    <div className="flex flex-wrap items-start gap-x-10 gap-y-4 px-4 pb-4">
      <DonutChart
        data={[...cats].map(([label, value]) => ({ label, value }))}
        size={130}
        thickness={18}
        centerLabel={formatCompact(
          [...cats.values()].reduce((s, v) => s + v, 0),
        )}
        centerSub="contracts"
      />
      <HBarList
        className="min-w-[16rem] flex-1"
        ariaLabel="Registered contracts per protocol"
        items={ranked.map((r) => ({
          label: r.name,
          value: r.contract_count,
        }))}
      />
    </div>
  );
}

/** Top counterparties by shared transactions, plus their protocol mix by shared txs. */
export function CounterpartyBars({
  edges,
  nameFor,
}: {
  edges: {
    contract_id?: string;
    protocol?: string | null;
    shared_txs?: number;
  }[];
  nameFor?: (id: string) => string | undefined;
}) {
  const items = edges
    .filter((e) => e.contract_id && (e.shared_txs ?? 0) > 0)
    .slice(0, 8)
    .map((e) => {
      const id = e.contract_id ?? '';
      return {
        id,
        label: nameFor?.(id) ?? `${id.slice(0, 6)}…${id.slice(-4)}`,
        value: e.shared_txs ?? 0,
        annotation: e.protocol ?? undefined,
        title: id,
      };
    });
  if (items.length === 0) return null;
  const slices = protocolSlices(edges, (e) => e.shared_txs ?? 0);
  return (
    <div className="space-y-1 px-4 pb-4">
      <div className="flex flex-wrap items-start gap-x-10 gap-y-4">
        {slices.some((d) => d.value > 0) && (
          <DonutChart
            data={slices}
            size={120}
            thickness={16}
            centerLabel={String(edges.length)}
            centerSub="counterparties"
            formatValue={formatCompact}
          />
        )}
        <HBarList
          className="min-w-[16rem] flex-1"
          ariaLabel="Top counterparties by shared transactions"
          items={items}
        />
      </div>
      <p className="text-ink-faint text-[11px]">
        Counts are lower bounds for busy contracts: the window narrows to the
        contract&apos;s recent activity.
      </p>
    </div>
  );
}

/** Exported entry points as chips; the signature rides in the title. */
export function ExportChips({
  exports,
}: {
  exports: { name: string; params: string[]; results: string[] }[];
}) {
  if (exports.length === 0) return null;
  return (
    <ul aria-label="Exported functions" className="flex flex-wrap gap-1.5">
      {exports.map((e) => (
        <li key={e.name}>
          <Badge
            tone="brand"
            className="font-mono"
            title={`(${e.params.join(', ')}) → ${e.results.length ? e.results.join(', ') : '()'}`}
          >
            {e.name}
          </Badge>
        </li>
      ))}
    </ul>
  );
}
