'use client';

import { useQuery } from '@tanstack/react-query';

import { Panel } from '@/components/reveal';
import { AssetText } from '@/components/AssetLink';
import { apiGet, asExample } from '@/api/client';
import { DonutChart } from '@/components/charts/DonutChart';
import { HBarList } from '@/components/charts/Bars';
import type { paths } from '@/api/types';

import { ReasonHeatmap } from './ReasonHeatmap';

// GET /v1/anomalies response body, derived from the generated OpenAPI
// contract (src/api/types.ts, `make web-generate-api`).
type AnomaliesResp = NonNullable<
  paths['/anomalies']['get']['responses'][200]['content']['application/json']['data']
>;
type FreezeEvent = NonNullable<AnomaliesResp['events']>[number];

// The window the reason tally + daily heatmap cover (the API default).
const TALLY_WINDOW_DAYS = 30;

function fmtTs(iso: string): string {
  const d = new Date(iso);
  return Number.isNaN(d.getTime()) ? iso : d.toISOString().replace('T', ' ').slice(0, 19) + 'Z';
}

function duration(from: string, to: string | null): string {
  const start = new Date(from).getTime();
  const end = to ? new Date(to).getTime() : Date.now();
  // Missing/garbage timestamps must render "—", never "NaNd" (same
  // finite-guard as lib/format's formatRelative).
  if (!Number.isFinite(start) || !Number.isFinite(end)) return '—';
  const s = Math.max(0, Math.round((end - start) / 1000));
  if (s < 90) return `${s}s`;
  if (s < 5400) return `${Math.round(s / 60)}m`;
  if (s < 129600) return `${Math.round(s / 3600)}h`;
  return `${Math.round(s / 86400)}d`;
}

// detail is a loose JSON bag on the wire (different freeze paths carry
// different fields); deviation_pct is the one worth a column.
function deviationPct(e: FreezeEvent): number | null {
  const det = e.detail as { deviation_pct?: unknown } | undefined;
  const n = det?.deviation_pct;
  return typeof n === 'number' && Number.isFinite(n) && n !== 0 ? n : null;
}

// Freeze-duration histogram buckets (recovered events only — a
// still-firing freeze has no closed duration and must not be
// counted as one).
const DURATION_BUCKETS: { label: string; maxSec: number }[] = [
  { label: '< 1 min', maxSec: 60 },
  { label: '1–10 min', maxSec: 600 },
  { label: '10–60 min', maxSec: 3600 },
  { label: '1–6 h', maxSec: 21_600 },
  { label: '6–24 h', maxSec: 86_400 },
  { label: '> 1 day', maxSec: Infinity },
];

export function AnomaliesFeed() {
  const q = useQuery<AnomaliesResp>({
    queryKey: ['/v1/anomalies'],
    queryFn: async () => {
      const env = await apiGet<{ data: AnomaliesResp }>('/v1/anomalies', { limit: 100, include: 'daily' });
      return env.data;
    },
    staleTime: 30_000,
    refetchInterval: 30_000,
  });

  const data = q.data;
  const events = data?.events ?? [];

  return (
    <>
      <AnomalyVisuals data={data} events={events} />
      <Panel
        headingLevel={2}
        title="Freeze timeline"
        hint="Every clear→firing transition from the durable freeze_events mirror, newest first."
        source={asExample('/v1/anomalies', { limit: 100, include: 'daily' })}
        bodyClassName="space-y-3"
      >
        {data && (
          <div className="flex flex-wrap gap-2 text-xs">
            <span
              className={`inline-flex items-center gap-1 rounded px-2 py-1 font-medium ${
                (data.firing_count ?? 0) > 0
                  ? 'bg-down-subtle text-down-strong'
                  : 'bg-up-subtle text-up-strong'
              }`}
            >
              {(data.firing_count ?? 0) > 0 ? `${data.firing_count} firing now` : 'Nothing firing'}
            </span>
            {(data.reason_tally ?? []).map((t) => (
              <span key={t.reason} className="inline-flex items-center gap-1 rounded-sm bg-surface-muted px-2 py-1 text-ink-body">
                <code className="font-mono">{t.reason}</code>
                <span className="text-ink-muted">×{t.count}</span>
              </span>
            ))}
            <span className="self-center text-ink-muted">(reasons: trailing 30d)</span>
          </div>
        )}
        {q.isLoading && <p className="text-sm text-ink-muted">Loading…</p>}
        {q.isError && <p className="text-sm text-ink-muted">The anomalies feed is unavailable right now.</p>}
        {data && events.length === 0 && (
          <p className="text-sm text-ink-muted">
            No freeze events recorded yet — every served price has cleared the anomaly checks.
          </p>
        )}
        {events.length > 0 && (
          // WCAG 1.4.10 Reflow: the wide mono columns scroll inside the
          // panel instead of pushing the whole page sideways at 320px.
          <div className="overflow-x-auto">
          <table className="w-full text-sm">
            <thead>
              <tr className="border-b border-line text-left text-[11px] uppercase tracking-wider text-ink-muted">
                <th className="py-1.5 pr-4 font-normal">Pair</th>
                <th className="py-1.5 pr-4 font-normal">Reason</th>
                <th className="py-1.5 pr-4 font-normal">Frozen at</th>
                <th className="py-1.5 pr-4 font-normal">Duration</th>
                <th className="py-1.5 pr-4 text-right font-normal">Deviation</th>
                <th className="py-1.5 pr-4 text-right font-normal">Frozen value</th>
                <th className="py-1.5 font-normal">State</th>
              </tr>
            </thead>
            <tbody>
              {events.map((e) => {
                const dev = deviationPct(e);
                return (
                  <tr
                    key={`${e.asset_id}:${e.quote_id}:${e.frozen_at}`}
                    className="border-b border-line/60 last:border-0 hover:bg-surface-muted"
                  >
                    <td className="py-1.5 pr-4 font-mono">
                      <AssetText canonical={e.asset_id} />
                      <span className="text-ink-faint">/</span>
                      <AssetText canonical={e.quote_id} />
                    </td>
                    <td className="py-1.5 pr-4">
                      <code className="text-[11px]">{e.reason}</code>
                    </td>
                    <td className="py-1.5 pr-4 font-mono text-[11px] text-ink-muted">{fmtTs(e.frozen_at ?? '')}</td>
                    <td className="py-1.5 pr-4 font-mono tabular-nums text-ink-muted">
                      {duration(e.frozen_at ?? '', e.recovered_at ?? null)}
                    </td>
                    <td
                      className="py-1.5 pr-4 text-right font-mono tabular-nums text-ink-muted"
                      title={dev != null ? 'detail.deviation_pct at freeze time' : 'no deviation recorded for this freeze path'}
                    >
                      {dev != null ? `${dev > 0 ? '+' : ''}${dev.toFixed(2)}%` : '—'}
                    </td>
                    <td className="py-1.5 pr-4 text-right font-mono tabular-nums">{e.frozen_value}</td>
                    <td className="py-1.5">
                      {e.firing ? (
                        <span className="rounded-sm bg-down-subtle px-1.5 py-0.5 text-[10px] font-medium uppercase text-down-strong">
                          firing
                        </span>
                      ) : (
                        <span className="rounded-sm bg-up-subtle px-1.5 py-0.5 text-[10px] font-medium uppercase text-up-strong">
                          recovered
                        </span>
                      )}
                    </td>
                  </tr>
                );
              })}
            </tbody>
          </table>
          </div>
        )}
      </Panel>
    </>
  );
}

// AnomalyVisuals — the visual pack over the SAME response the feed
// already fetches: reason composition (donut, 30d tally), freeze-
// duration histogram (recovered events among the page served), and
// the day×reason calendar heatmap (the `?include=daily` block).
// Every sub-visual renders only over data actually served — a
// missing daily block (older API / degraded reader) renders nothing
// rather than a grid of fabricated zeros.
function AnomalyVisuals({ data, events }: { data?: AnomaliesResp; events: FreezeEvent[] }) {
  const tally = (data?.reason_tally ?? []).filter((t) => (t.count ?? 0) > 0);
  const donut = tally.map((t) => ({ label: t.reason ?? 'unknown', value: t.count ?? 0 }));

  const recovered = events.filter((e) => !e.firing && e.recovered_at && e.frozen_at);
  const counts = DURATION_BUCKETS.map(() => 0);
  for (const e of recovered) {
    const sec = (new Date(e.recovered_at as string).getTime() - new Date(e.frozen_at as string).getTime()) / 1000;
    if (!Number.isFinite(sec) || sec < 0) continue;
    counts[DURATION_BUCKETS.findIndex((b) => sec < b.maxSec)] += 1;
  }
  const histogram = DURATION_BUCKETS.map((b, i) => ({ label: b.label, value: counts[i] })).filter(
    (x) => x.value > 0,
  );

  // daily is null when not requested/served, [] when served-and-empty
  // — only a real array may drive the heatmap (degraded ≠ zero).
  const daily = data?.daily;
  const heatCells = Array.isArray(daily)
    ? daily.map((d) => ({ day: d.day ?? '', reason: d.reason ?? '', count: d.count ?? 0 }))
    : null;

  if (donut.length === 0 && histogram.length === 0 && heatCells == null) return null;

  return (
    <>
      <div className="grid gap-6 lg:grid-cols-2">
        {donut.length > 0 && (
          <Panel
            headingLevel={2}
            title="Freezes by reason — 30d"
            hint="Composition of the trailing-30-day freeze tally by trigger reason."
            source={asExample('/v1/anomalies', { limit: 100, include: 'daily' })}
          >
            <DonutChart
              data={donut}
              centerLabel={donut.reduce((s, d) => s + d.value, 0).toLocaleString('en-US')}
              centerSub="freezes 30d"
            />
          </Panel>
        )}
        {histogram.length > 0 && (
          <Panel
            headingLevel={2}
            title="Freeze duration"
            hint={`How long pairs stayed dark before recovering — the ${recovered.length} recovered freeze${
              recovered.length === 1 ? '' : 's'
            } among the ${events.length} most recent events served. Still-firing freezes are excluded (no closed duration yet).`}
            source={asExample('/v1/anomalies', { limit: 100, include: 'daily' })}
          >
            <HBarList
              items={histogram}
              ariaLabel="Histogram of freeze durations among recently recovered freeze events"
            />
          </Panel>
        )}
      </div>
      {heatCells != null && (
        <Panel
          headingLevel={2}
          title="Freeze calendar — day × reason, 30d"
          hint="Each cell is one UTC day for one trigger reason; darker = more freezes. Clustered columns are incidents, an even wash is background noise."
          source={asExample('/v1/anomalies', { limit: 100, include: 'daily' })}
        >
          <ReasonHeatmap cells={heatCells} windowDays={TALLY_WINDOW_DAYS} />
        </Panel>
      )}
    </>
  );
}
