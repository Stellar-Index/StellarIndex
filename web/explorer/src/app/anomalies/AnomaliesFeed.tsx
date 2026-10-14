'use client';

import { useQuery } from '@tanstack/react-query';

import { Panel } from '@/components/reveal';
import { Badge, TBody, TR, Table, Td, Th, THead } from '@/components/ui';
import { AssetText } from '@/components/AssetLink';
import { apiGet, asExample } from '@/api/client';
import { formatPairPrice } from '@/lib/format';
import { DonutChart } from '@/components/charts/DonutChart';
import { HBarList } from '@/components/charts/Bars';
import type { paths } from '@/api/types';

import { FreezeGantt } from './FreezeGantt';
import { ReasonHeatmap } from './ReasonHeatmap';

// GET /v1/anomalies response body, derived from the generated OpenAPI
// contract (src/api/types.ts, `make web-generate-api`).
type AnomaliesResp = NonNullable<
  paths['/anomalies']['get']['responses'][200]['content']['application/json']['data']
>;
type FreezeEvent = NonNullable<AnomaliesResp['events']>[number];

// The window the reason tally + daily heatmap cover (the API default).
const TALLY_WINDOW_DAYS = 30;

const FREEZE_CSV_COLUMNS = [
  'asset_id',
  'quote_id',
  'reason',
  'frozen_at',
  'frozen_at_ledger',
  'frozen_value',
  'recovered_at',
  'recovered_at_ledger',
  'firing',
] as const;

function fmtTs(iso: string): string {
  const d = new Date(iso);
  return Number.isNaN(d.getTime())
    ? iso
    : d.toISOString().replace('T', ' ').slice(0, 19) + 'Z';
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
      const env = await apiGet<{ data: AnomaliesResp }>('/v1/anomalies', {
        limit: 100,
        include: 'daily',
      });
      return env.data;
    },
    staleTime: 30_000,
    refetchInterval: 30_000,
  });

  const data = q.data;
  const events = data?.events ?? [];

  return (
    <>
      <AnomalyVisuals data={data} events={events} fetchedAt={q.dataUpdatedAt} />
      <Panel
        headingLevel={2}
        title="Freeze timeline"
        hint="Every clear→firing transition from the durable freeze_events mirror, newest first."
        source={asExample('/v1/anomalies', { limit: 100, include: 'daily' })}
        download={
          events.length > 0
            ? {
                name: 'price-freezes',
                columns: FREEZE_CSV_COLUMNS,
                rows: events.map((e) => ({
                  asset_id: e.asset_id,
                  quote_id: e.quote_id,
                  reason: e.reason,
                  frozen_at: e.frozen_at,
                  frozen_at_ledger: e.frozen_at_ledger,
                  frozen_value: e.frozen_value,
                  recovered_at: e.recovered_at,
                  recovered_at_ledger: e.recovered_at_ledger,
                  firing: e.firing,
                })),
              }
            : undefined
        }
        bodyClassName="-mx-4 -mb-4"
      >
        {data && (
          <div className="mx-4 mb-4 flex flex-wrap items-center gap-2 text-xs">
            <Badge tone={(data.firing_count ?? 0) > 0 ? 'bad' : 'ok'} dot>
              {(data.firing_count ?? 0) > 0
                ? `${data.firing_count} firing now`
                : 'Nothing firing'}
            </Badge>
            {(data.reason_tally ?? []).map((t) => (
              <Badge key={t.reason}>
                <code className="font-mono">{t.reason}</code>
                <span className="text-ink-muted ml-1">×{t.count}</span>
              </Badge>
            ))}
            <span className="text-ink-muted">(reasons: trailing 30d)</span>
          </div>
        )}
        {q.isLoading && (
          <p className="text-ink-muted px-4 pb-4 text-sm">Loading…</p>
        )}
        {q.isError && (
          <p className="text-ink-muted px-4 pb-4 text-sm">
            The anomalies feed is unavailable right now.
          </p>
        )}
        {data && events.length === 0 && (
          <p className="text-ink-muted px-4 pb-4 text-sm">
            No freeze events recorded yet — every served price has cleared the
            anomaly checks.
          </p>
        )}
        {events.length > 0 && (
          // WCAG 1.4.10 Reflow: the wide mono columns scroll inside the
          // panel instead of pushing the whole page sideways at 320px.
          <div className="overflow-x-auto">
            <Table>
              <THead>
                <tr>
                  <Th>Pair</Th>
                  <Th>Reason</Th>
                  <Th>Frozen at</Th>
                  <Th>Duration</Th>
                  <Th align="right">Deviation</Th>
                  <Th align="right">Frozen value</Th>
                  <Th>State</Th>
                </tr>
              </THead>
              <TBody>
                {events.map((e) => {
                  const dev = deviationPct(e);
                  return (
                    <TR key={`${e.asset_id}:${e.quote_id}:${e.frozen_at}`}>
                      <Td className="font-mono whitespace-nowrap">
                        <AssetText canonical={e.asset_id} />
                        <span className="text-ink-faint">/</span>
                        <AssetText canonical={e.quote_id} />
                      </Td>
                      <Td>
                        <code className="text-xs">{e.reason}</code>
                      </Td>
                      <Td className="text-ink-muted font-mono text-xs whitespace-nowrap">
                        {fmtTs(e.frozen_at ?? '')}
                      </Td>
                      <Td className="text-ink-muted tnum font-mono">
                        {duration(e.frozen_at ?? '', e.recovered_at ?? null)}
                      </Td>
                      <Td
                        align="right"
                        className="text-ink-muted font-mono"
                        title={
                          dev != null
                            ? 'detail.deviation_pct at freeze time'
                            : 'no deviation recorded for this freeze path'
                        }
                      >
                        {dev != null
                          ? `${dev > 0 ? '+' : ''}${dev.toFixed(2)}%`
                          : '—'}
                      </Td>
                      <Td
                        align="right"
                        className="font-mono"
                        title={e.frozen_value}
                      >
                        {e.frozen_value ? formatPairPrice(e.frozen_value) : '—'}
                      </Td>
                      <Td>
                        {e.firing ? (
                          <Badge tone="bad">Firing</Badge>
                        ) : (
                          <Badge tone="ok">Recovered</Badge>
                        )}
                      </Td>
                    </TR>
                  );
                })}
              </TBody>
            </Table>
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
function AnomalyVisuals({
  data,
  events,
  fetchedAt,
}: {
  data?: AnomaliesResp;
  events: FreezeEvent[];
  fetchedAt: number;
}) {
  const tally = (data?.reason_tally ?? []).filter((t) => (t.count ?? 0) > 0);
  const donut = tally.map((t) => ({
    label: t.reason ?? 'unknown',
    value: t.count ?? 0,
  }));

  const recovered = events.filter(
    (e) => !e.firing && e.recovered_at && e.frozen_at,
  );
  const counts = DURATION_BUCKETS.map(() => 0);
  for (const e of recovered) {
    const sec =
      (new Date(e.recovered_at as string).getTime() -
        new Date(e.frozen_at as string).getTime()) /
      1000;
    if (!Number.isFinite(sec) || sec < 0) continue;
    counts[DURATION_BUCKETS.findIndex((b) => sec < b.maxSec)] += 1;
  }
  const histogram = DURATION_BUCKETS.map((b, i) => ({
    label: b.label,
    value: counts[i],
  })).filter((x) => x.value > 0);

  // daily is null when not requested/served, [] when served-and-empty
  // — only a real array may drive the heatmap (degraded ≠ zero).
  const daily = data?.daily;
  const heatCells = Array.isArray(daily)
    ? daily.map((d) => ({
        day: d.day ?? '',
        reason: d.reason ?? '',
        count: d.count ?? 0,
      }))
    : null;

  if (
    events.length === 0 &&
    donut.length === 0 &&
    histogram.length === 0 &&
    heatCells == null
  )
    return null;

  return (
    <>
      <div className="grid gap-6 lg:grid-cols-2">
        {donut.length > 0 && (
          <Panel
            headingLevel={2}
            title="Freezes by reason — 30d"
            hint="Composition of the trailing-30-day freeze tally by trigger reason."
            source={asExample('/v1/anomalies', {
              limit: 100,
              include: 'daily',
            })}
          >
            <DonutChart
              data={donut}
              centerLabel={donut
                .reduce((s, d) => s + d.value, 0)
                .toLocaleString('en-US')}
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
            source={asExample('/v1/anomalies', {
              limit: 100,
              include: 'daily',
            })}
          >
            <HBarList
              items={histogram}
              ariaLabel="Histogram of freeze durations among recently recovered freeze events"
            />
          </Panel>
        )}
      </div>
      {events.length > 0 && (
        <Panel
          headingLevel={2}
          title="Freezes by pair"
          hint="One row per pair, most recently frozen first. Red is still firing; amber recovered."
          source={asExample('/v1/anomalies', { limit: 100, include: 'daily' })}
        >
          <FreezeGantt events={events} nowMs={fetchedAt} />
        </Panel>
      )}
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
