import { Badge } from '@/components/ui';
import { HBarList } from '@/components/charts/Bars';
import { DonutChart } from '@/components/charts/DonutChart';
import { Sparkline } from '@/components/primitives/Sparkline';

const OPS_BUCKETS: { label: string; test: (n: number) => boolean }[] = [
  { label: '1 op', test: (n) => n <= 1 },
  { label: '2–5 ops', test: (n) => n >= 2 && n <= 5 },
  { label: '6–20 ops', test: (n) => n >= 6 && n <= 20 },
  { label: '21+ ops', test: (n) => n > 20 },
];

/** Applied vs failed transactions of one ledger. */
export function ResultDonut({ ok, failed }: { ok: number; failed: number }) {
  return (
    <DonutChart
      size={120}
      thickness={16}
      centerLabel={(ok + failed).toLocaleString('en-US')}
      centerSub="txs"
      data={[
        { label: 'Successful', value: ok, color: 'var(--color-up)' },
        { label: 'Failed', value: failed, color: 'var(--color-down)' },
      ]}
    />
  );
}

/** Histogram of transactions by operation count. */
export function OpsPerTxBars({ opCounts }: { opCounts: number[] }) {
  return (
    <HBarList
      ariaLabel="Transactions by operation count"
      items={OPS_BUCKETS.map((b) => ({
        label: b.label,
        value: opCounts.filter(b.test).length,
      }))}
    />
  );
}

/** Per-ledger count series, oldest first, with an accessible summary. */
export function CountSparkline({
  values,
  label,
  noun = 'ledgers',
}: {
  values: number[];
  label: string;
  noun?: string;
}) {
  if (values.length < 2) return null;
  return (
    <div
      role="img"
      aria-label={`${label}: ${values.length} ${noun}, min ${Math.min(...values).toLocaleString('en-US')}, max ${Math.max(...values).toLocaleString('en-US')}`}
      title={label}
    >
      <Sparkline values={values} width={160} height={28} tone="neutral" />
    </div>
  );
}

/** Whole seconds between adjacent ledger closes, oldest first; skips gaps in sequence. */
export function closeIntervals(
  ledgers: { sequence?: number; close_time?: string }[],
): number[] {
  const asc = [...ledgers].sort(
    (a, b) => (a.sequence ?? 0) - (b.sequence ?? 0),
  );
  const out: number[] = [];
  for (let i = 1; i < asc.length; i++) {
    const a = asc[i - 1];
    const b = asc[i];
    if (a.sequence == null || b.sequence !== a.sequence + 1) continue;
    const t0 = Date.parse(a.close_time ?? '');
    const t1 = Date.parse(b.close_time ?? '');
    if (Number.isNaN(t0) || Number.isNaN(t1) || t1 < t0) continue;
    out.push(Math.round((t1 - t0) / 1000));
  }
  return out;
}

const isInt = (s: string | undefined): s is string => !!s && /^\d+$/.test(s);

/** Fee charged as a share of the bid; exact BigInt maths, bar is geometry. */
export function feeUsage(
  charged: string | undefined,
  max: string | undefined,
): { pctTenths: number; headroom: string } | null {
  if (!isInt(charged) || !isInt(max)) return null;
  const c = BigInt(charged);
  const m = BigInt(max);
  if (m <= 0n) return null;
  const used = c > m ? m : c;
  return {
    pctTenths: Number((used * 1000n) / m),
    headroom: (m - used).toString(),
  };
}

export function FeeHeadroomBar({
  charged,
  max,
  format,
}: {
  charged?: string;
  max?: string;
  format: (stroops: string) => string;
}) {
  const u = feeUsage(charged, max);
  if (!u) return null;
  const pct = (u.pctTenths / 10).toFixed(1);
  return (
    <div className="space-y-1">
      <div
        role="img"
        aria-label={`Fee charged is ${pct}% of the max fee bid`}
        title={`${pct}% of max fee used`}
        className="bg-surface-subtle h-2 w-full overflow-hidden rounded-xs"
      >
        <div
          className="h-full"
          style={{
            width: `${u.pctTenths / 10}%`,
            backgroundColor: 'var(--color-brand-500)',
          }}
        />
      </div>
      <p className="text-ink-muted text-xs">
        {pct}% of max fee used · {format(u.headroom)} XLM headroom
      </p>
    </div>
  );
}

/** Operation types of one transaction as counted badges. */
export function OpTypeStrip({ types }: { types: string[] }) {
  const counts = new Map<string, number>();
  for (const t of types) counts.set(t, (counts.get(t) ?? 0) + 1);
  if (counts.size === 0) return null;
  return (
    <ul aria-label="Operation types" className="flex flex-wrap gap-1.5">
      {[...counts].map(([t, n]) => (
        <li key={t}>
          <Badge tone="brand">{n > 1 ? `${t} ×${n}` : t}</Badge>
        </li>
      ))}
    </ul>
  );
}

/** Protocol upgrades in the window as dated badges. */
export function UpgradeBadges({
  markers,
}: {
  markers: { time: number; label: string }[];
}) {
  if (markers.length === 0) return null;
  return (
    <ul
      aria-label="Protocol upgrades in this window"
      className="flex flex-wrap gap-1.5"
    >
      {markers.map((m) => {
        const day = new Date(m.time * 1000).toISOString().slice(0, 10);
        return (
          <li key={`${m.label}-${m.time}`}>
            <Badge tone="warn" title={`${m.label} on ${day}`}>
              {m.label} · {day}
            </Badge>
          </li>
        );
      })}
    </ul>
  );
}
