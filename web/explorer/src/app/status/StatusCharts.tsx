import { HBarList, type HBarItem } from '@/components/charts/Bars';
import { DonutChart } from '@/components/charts/DonutChart';
import { formatCompact } from '@/lib/format';

export type LatencySample = {
  path: string;
  latencyMs: number;
  tone: 'ok' | 'warn' | 'bad';
};

const TONE_COLOR = {
  ok: 'var(--color-ok-500)',
  warn: 'var(--color-warn-500)',
  bad: 'var(--color-bad-500)',
} as const;

/** Slowest probed endpoints first; unmeasured probes (latency < 0) are left out. */
export function EndpointLatencyBars({
  samples,
  limit = 10,
}: {
  samples: LatencySample[];
  limit?: number;
}) {
  const items: HBarItem[] = samples
    .filter((s) => Number.isFinite(s.latencyMs) && s.latencyMs >= 0)
    .sort((a, b) => b.latencyMs - a.latencyMs)
    .slice(0, limit)
    .map((s) => ({
      label: s.path,
      value: s.latencyMs,
      display: `${Math.round(s.latencyMs)} ms`,
      color: TONE_COLOR[s.tone],
    }));
  if (items.length === 0) return null;
  return (
    <div className="mb-5">
      <h3 className="text-ink-muted mb-2 text-[11px] font-semibold tracking-wider uppercase">
        Slowest endpoints
      </h3>
      <HBarList
        items={items}
        ariaLabel="Slowest probed endpoints by response time"
      />
    </div>
  );
}

export type SourceRow = {
  name: string;
  class: string;
  enabled?: boolean;
  volume_24h_usd?: string | null;
};

/** Per-source 24h USD volume bars (top N) plus a class-mix donut. */
export function SourceVolumeCharts({
  rows,
  limit = 10,
}: {
  rows: SourceRow[];
  limit?: number;
}) {
  const withVolume = rows
    .map((r) => ({ r, v: Number(r.volume_24h_usd) }))
    .filter(({ r, v }) => r.enabled !== false && Number.isFinite(v) && v > 0)
    .sort((a, b) => b.v - a.v);
  const bars: HBarItem[] = withVolume.slice(0, limit).map(({ r, v }) => ({
    label: r.name,
    value: v,
    display: `$${formatCompact(v)}`,
  }));
  const mix = new Map<string, number>();
  for (const r of rows) mix.set(r.class, (mix.get(r.class) ?? 0) + 1);
  const slices = Array.from(mix, ([label, value]) => ({ label, value }));
  if (bars.length === 0 && slices.length < 2) return null;
  const hidden = withVolume.length - bars.length;
  return (
    <div className="mb-4 grid gap-6 md:grid-cols-[1fr_auto]">
      {bars.length > 0 && (
        <div>
          <h3 className="text-ink-muted mb-2 text-[11px] font-semibold tracking-wider uppercase">
            24h volume by source
            {hidden > 0 ? ` — top ${bars.length} of ${withVolume.length}` : ''}
          </h3>
          <HBarList
            items={bars}
            ariaLabel="24h USD volume by source, top sources only"
          />
        </div>
      )}
      {slices.length > 1 && (
        <DonutChart
          data={slices}
          centerLabel={String(rows.length)}
          centerSub="sources"
        />
      )}
    </div>
  );
}
