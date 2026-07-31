'use client';

import { useMemo, useState } from 'react';
import dynamic from 'next/dynamic';
import Link from 'next/link';
import { useQuery } from '@tanstack/react-query';

import { Panel } from '@/components/reveal';
import { apiGet, type RequestExample } from '@/api/client';
import { cn } from '@/lib/cn';
import { formatCompact } from '@/lib/format';
import type { NamedLineSeries } from '@/components/charts/LineChart';
import { CATEGORICAL_PALETTE } from '@/components/charts/DonutChart';
import { dropPartialTrailingDay, seriesPointTime } from '@/lib/series';
import { CopyHash } from '../../explorer-shared';
import { TimeSeriesChart, type ChartTone } from './TimeSeriesChart';
import {
  BridgeShowcase,
  BreakdownDonuts,
  LineLegend,
} from './BridgeShowcase';

const LineChart = dynamic(
  () => import('@/components/charts/LineChart').then((m) => m.LineChart),
  { ssr: false, loading: () => <div className="h-56 w-full" /> },
);

// ─── Wire shapes (mirror internal/api/v1/protocols.go ProtocolBespoke) ───
//
// A GENERIC rendering container — KPIs + named time-series + named top-N
// tables + notes — filled bespoke per category server-side. The UI renders
// the three shapes generically, so a new category's metrics are a server data
// change, not a UI layout change. Every sub-part is optional and renders only
// when present (graceful degradation).

export interface BespokeKpi {
  label: string;
  value: string; // PRE-FORMATTED server-side (ADR-0003-safe).
  unit?: string;
  hint?: string;
}

export interface BespokeSeriesPt {
  date: string; // YYYY-MM-DD
  value: string; // numeric string — Number() for geometry only.
}

export interface BespokeSeries {
  name: string;
  unit?: string;
  points: BespokeSeriesPt[];
}

export interface BespokeTable {
  title: string;
  columns: string[];
  rows: string[][];
}

export interface BespokeBreakdownRow {
  label: string;
  value: string; // numeric string — Number() for geometry only.
  count: number;
}

export interface BespokeBreakdown {
  title: string;
  unit?: string;
  rows: BespokeBreakdownRow[];
}

export interface Bespoke {
  category: string;
  kpis?: BespokeKpi[];
  series?: BespokeSeries[];
  breakdowns?: BespokeBreakdown[];
  tables?: BespokeTable[];
  notes?: string[];
}

// category → headline label + chart palette. Falls through to a neutral
// "Protocol analytics" so an unknown server-side category still renders.
const CATEGORY_LABEL: Record<string, string> = {
  dex: 'DEX analytics',
  amm: 'AMM analytics',
  lending: 'Lending analytics',
  vault: 'Vault analytics',
  yield: 'Vault analytics',
  bridge: 'Bridge analytics',
  oracle: 'Oracle analytics',
};

// Rotating chart tones so multiple series on one page read distinctly.
const SERIES_TONES: ChartTone[] = [
  'emerald',
  'brand',
  'violet',
  'amber',
  'indigo',
];

function categoryLabel(category: string): string {
  return CATEGORY_LABEL[category] ?? 'Protocol analytics';
}

// ─── Section-level analytics window ──────────────────────────────────────
//
// The 24h/7d/30d/90d pills live at BespokeSection level (lifted out of the
// bridge showcase, 2026-07-30) so EVERY category's whole bespoke block —
// KPIs, series, donuts, tables — is window-reactive. Switching a pill
// refetches `/v1/protocols/{name}?days=N`; the server keys its detail
// cache on (name, days) and buckets the 24h window hourly.

export const WINDOWS = [
  { key: '24h', days: 1 },
  { key: '7d', days: 7 },
  { key: '30d', days: 30 },
  { key: '90d', days: 90 },
] as const;

export type WindowDays = (typeof WINDOWS)[number]['days'];

/** The page's initial fetch covers the server default window (90d). */
export const DEFAULT_WINDOW_DAYS: WindowDays = 90;

export function windowLabelFor(days: WindowDays): string {
  return WINDOWS.find((w) => w.days === days)?.key ?? `${days}d`;
}

/** The shared 24h/7d/30d/90d pill row. */
function WindowPills({
  days,
  onChange,
}: {
  days: WindowDays;
  onChange: (d: WindowDays) => void;
}) {
  return (
    <div role="group" aria-label="Analytics window" className="flex gap-1">
      {WINDOWS.map((w) => (
        <button
          key={w.key}
          type="button"
          aria-pressed={days === w.days}
          onClick={() => onChange(w.days)}
          className={cn(
            'rounded-sm px-2 py-0.5 font-mono text-[11px] focus-visible:outline-hidden focus-visible:ring-2 focus-visible:ring-brand-500/60',
            days === w.days
              ? 'bg-brand-100 text-brand-700'
              : 'text-ink-muted hover:text-ink-body',
          )}
        >
          {w.key}
        </button>
      ))}
    </div>
  );
}

// ─── Series grouping ─────────────────────────────────────────────────────

/**
 * splitSeriesGroups — partition a bespoke series list into standalone
 * series (one chart panel each) and prefix groups: series named
 * "<Group> · <entity>" (the DEX "Top pairs · XLM/USDC" convention, same
 * separator as the bridge's per-chain lines) fold into one multi-line
 * panel per group so entity lines share a pane instead of exploding into
 * five separate panels.
 */
export function splitSeriesGroups(series: BespokeSeries[]): {
  standalone: BespokeSeries[];
  groups: { title: string; series: BespokeSeries[] }[];
} {
  const standalone: BespokeSeries[] = [];
  const groups: { title: string; series: BespokeSeries[] }[] = [];
  const byTitle = new Map<string, { title: string; series: BespokeSeries[] }>();
  for (const s of series) {
    const idx = s.name.indexOf(' · ');
    if (idx <= 0) {
      standalone.push(s);
      continue;
    }
    const title = s.name.slice(0, idx);
    let g = byTitle.get(title);
    if (!g) {
      g = { title, series: [] };
      byTitle.set(title, g);
      groups.push(g);
    }
    g.series.push(s);
  }
  return { standalone, groups };
}

// GroupedSeriesPanel — one multi-line chart per series group, lines
// palette-colored in the server's order (volume-descending for the DEX
// top pairs) and labelled by the bare entity name.
function GroupedSeriesPanel({
  group,
  source,
  days,
}: {
  group: { title: string; series: BespokeSeries[] };
  source: RequestExample;
  days: WindowDays;
}) {
  const lines: NamedLineSeries[] = group.series.map((s, i) => ({
    label: s.name.slice(group.title.length + ' · '.length),
    tone: 'brand',
    color: CATEGORICAL_PALETTE[i % CATEGORICAL_PALETTE.length],
    // Today's accumulating daily bucket is dropped (phantom-cliff honesty);
    // points with a non-parsable date or non-numeric value are dropped, not
    // plotted as 1970/zero fabrications.
    data: dropPartialTrailingDay(s.points).flatMap((p) => {
      const time = seriesPointTime(p.date);
      const value = toChartNumber(p.value);
      return time == null || value == null ? [] : [{ time, value }];
    }),
  }));
  const unit = group.series[0]?.unit;
  const grainLabel = days === 1 ? 'hourly' : 'daily';
  return (
    <Panel
      title={group.title}
      hint={`${unit ? `Units: ${unit} · ` : ''}${grainLabel} · last ${windowLabelFor(days)}`}
      source={source}
    >
      <div className="space-y-3">
        <LineLegend lines={lines} />
        <LineChart
          data={[]}
          series={lines}
          height={224}
          timeVisible={days === 1}
          legend={{
            valueLabel: lines[0]?.label ?? '',
            formatValue: formatCompact,
          }}
          ariaLabel={`${group.title}: ${lines.length} series over the last ${windowLabelFor(days)}${unit ? `, in ${unit}` : ''}.`}
        />
      </div>
    </Panel>
  );
}

/**
 * BespokeSection — the category-specific, Dune-surpassing analytics block.
 * Renders ONLY the sub-parts the server populated; the whole section is
 * omitted upstream when `data.bespoke` is absent. Distinct visual treatment
 * (gradient header band + accent KPI cards) so it reads as the protocol's
 * tailored headline rather than the generic activity panels around it.
 *
 * The section OWNS the analytics window: the 24h/7d/30d/90d pills in the
 * header refetch `/v1/protocols/{name}?days=N` and swap the WHOLE block
 * (KPIs included — the server labels them "(7d)" etc. per window). The
 * default window reuses the block the page already fetched — no duplicate
 * request on first render. Bridge blocks delegate their chart suite to
 * BridgeShowcase, which consumes this section-level window instead of
 * owning pills (lifted 2026-07-30); its all-time cumulative headline stays
 * pinned to the initial fetch across pill switches.
 */
export function BespokeSection({
  bespoke,
  source,
  name,
  omitSeries,
}: {
  bespoke: Bespoke;
  source: RequestExample;
  /** Protocol slug — the window refetch re-hits /v1/protocols/{name}. */
  name: string;
  /**
   * Optional series filter: series whose name matches are NOT rendered
   * (applies to window refetches too). Used by the /dexes/[source] host,
   * which renders the standalone USD-volume series in its own panel and
   * would otherwise show it twice.
   */
  omitSeries?: (name: string) => boolean;
}) {
  const [days, setDays] = useState<WindowDays>(DEFAULT_WINDOW_DAYS);

  // Non-default windows refetch the protocol detail with ?days=N.
  const { data, isFetching, isError } = useQuery<Bespoke | null>({
    queryKey: ['/v1/protocols/{name}', name, 'bespoke-window', days],
    enabled: days !== DEFAULT_WINDOW_DAYS,
    retry: false,
    staleTime: 60_000,
    queryFn: async () => {
      const env = await apiGet<{ data?: { bespoke?: Bespoke } }>(
        `/v1/protocols/${encodeURIComponent(name)}?days=${days}`,
      );
      return env.data?.bespoke ?? null;
    },
  });

  const loading = days !== DEFAULT_WINDOW_DAYS && isFetching;
  const failed = days !== DEFAULT_WINDOW_DAYS && isError;
  const active: Bespoke | null =
    days === DEFAULT_WINDOW_DAYS
      ? bespoke
      : loading || failed
        ? null
        : (data ?? null);

  const isBridge = bespoke.category === 'bridge';
  const activeSeries = useMemo(() => {
    const all = active?.series ?? [];
    return omitSeries ? all.filter((s) => !omitSeries(s.name)) : all;
  }, [active, omitSeries]);
  const { standalone, groups } = useMemo(
    () => splitSeriesGroups(activeSeries),
    [activeSeries],
  );

  // Initial-block presence decides the layout (an empty refetched window
  // must not collapse the section — the pills need to stay reachable).
  const hasKpis = (bespoke.kpis?.length ?? 0) > 0;
  const hasSeries = (bespoke.series?.length ?? 0) > 0;
  const hasBreakdowns = (bespoke.breakdowns?.length ?? 0) > 0;
  const hasTables = (bespoke.tables?.length ?? 0) > 0;
  // Notes belong to the ACTIVE window's block only — the initial (90d)
  // block's caveats must not silently caption another window's data
  // (they vanish while a refetch is in flight or failed, which is honest).
  const notes = active?.notes ?? [];

  // Nothing to show beyond the heading → don't render an empty band.
  if (
    !hasKpis &&
    !hasSeries &&
    !hasBreakdowns &&
    !hasTables &&
    (bespoke.notes?.length ?? 0) === 0
  ) {
    return null;
  }

  const activeKpis = active?.kpis ?? [];

  return (
    <section
      aria-labelledby="bespoke-heading"
      className="space-y-4 rounded-xl border border-brand-100 bg-linear-to-b from-brand-50/60 to-transparent p-4"
    >
      {/* ── Category chip + heading + the shared window pills ── */}
      <div className="flex flex-wrap items-center gap-2">
        <span className="rounded-sm bg-brand-100 px-2 py-0.5 font-mono text-[11px] uppercase tracking-wider text-brand-700">
          {bespoke.category}
        </span>
        <h2
          id="bespoke-heading"
          className="text-lg font-semibold tracking-tight"
        >
          {categoryLabel(bespoke.category)}
        </h2>
        <span className="text-xs text-ink-muted">
          tailored on-chain metrics for this protocol
        </span>
        <div className="ml-auto">
          <WindowPills days={days} onChange={setDays} />
        </div>
      </div>

      {/* ── Bespoke KPI cards (window-reactive with the rest) ── */}
      {loading && hasKpis ? (
        <div
          aria-hidden
          className="h-24 w-full animate-pulse rounded-lg bg-surface-subtle"
          data-testid="bespoke-kpis-loading"
        />
      ) : (
        activeKpis.length > 0 && (
          <div className="grid grid-cols-2 gap-3 sm:grid-cols-3 lg:grid-cols-4">
            {activeKpis.map((k, i) => (
              <BespokeKpiCard key={`${k.label}-${i}`} kpi={k} />
            ))}
          </div>
        )
      )}

      {/* ── Bespoke charts ── */}
      {/* Bridge blocks hand their whole chart suite (flow series, per-chain
          lines, donuts, cumulative net inflow, largest-transfers table) to
          BridgeShowcase, driven by this section's window; every other
          category renders standalone series one panel each, "<Group> ·
          <entity>" series as one multi-line panel per group, plus generic
          breakdown donuts/tables — all from the active window's block. */}
      {isBridge && (hasSeries || hasBreakdowns || hasTables) ? (
        <BridgeShowcase
          initial={bespoke}
          active={active}
          days={days}
          isFetching={loading}
          isError={failed}
          source={source}
        />
      ) : failed ? (
        <p className="py-6 text-center text-sm text-ink-muted">
          Couldn&apos;t load this window — retry, or pick another window.
        </p>
      ) : loading ? (
        <div
          aria-hidden
          className="h-56 w-full animate-pulse rounded-md bg-surface-subtle"
          data-testid="bespoke-window-loading"
        />
      ) : active === null ? (
        <p className="py-6 text-center text-sm text-ink-muted">
          No analytics in this window.
        </p>
      ) : (
        <>
          {standalone.map((s, i) => (
            <Panel
              key={`${s.name}-${i}`}
              title={s.name}
              hint={`${s.unit ? `Units: ${s.unit} · ` : ''}last ${windowLabelFor(days)}`}
              source={source}
            >
              <TimeSeriesChart
                points={dropPartialTrailingDay(s.points).flatMap((p) => {
                  const value = toChartNumber(p.value);
                  return value == null ? [] : [{ date: p.date, value }];
                })}
                label={s.name}
                unit={s.unit}
                tone={SERIES_TONES[i % SERIES_TONES.length]}
                gradientId={`bespokeSeries-${i}`}
                timeVisible={days === 1}
              />
            </Panel>
          ))}

          {groups.map((g) => (
            <GroupedSeriesPanel
              key={g.title}
              group={g}
              source={source}
              days={days}
            />
          ))}

          {/* ── Generic breakdown donuts (categories that adopt the
                 breakdowns wire field — DEX ships "Volume by pair") ── */}
          {(active.breakdowns?.length ?? 0) > 0 && (
            <BreakdownDonuts
              breakdowns={active.breakdowns!}
              source={source}
              windowLabel={windowLabelFor(days)}
            />
          )}

          {(active.tables ?? []).map((t, i) => (
            <BespokeTablePanel
              key={`${t.title}-${i}`}
              table={t}
              source={source}
            />
          ))}
        </>
      )}

      {/* ── Notes / caveats ── */}
      {notes.length > 0 && (
        <ul className="space-y-1 px-1 text-xs text-ink-muted">
          {notes.map((n, i) => (
            <li key={i} className="flex gap-1.5">
              <span aria-hidden className="select-none text-ink-faint">
                ·
              </span>
              <span>{n}</span>
            </li>
          ))}
        </ul>
      )}
    </section>
  );
}

// ─── Bespoke KPI card — accent styling, distinct from the generic KPI row ──

function BespokeKpiCard({ kpi }: { kpi: BespokeKpi }) {
  return (
    <div className="rounded-lg border border-brand-200/70 bg-surface/80 p-3 shadow-sm">
      <div
        className="flex items-center gap-1 text-[10px] uppercase tracking-wider text-ink-muted"
        title={kpi.hint || undefined}
      >
        <span className="truncate">{kpi.label}</span>
        {kpi.hint && (
          <span
            aria-hidden
            className="cursor-help text-ink-faint"
            title={kpi.hint}
          >
            ⓘ
          </span>
        )}
      </div>
      <div className="mt-1 flex items-baseline gap-1">
        <span className="text-2xl font-semibold tabular-nums text-brand-700">
          {kpi.value}
        </span>
        {kpi.unit && (
          <span className="text-xs font-medium text-ink-muted">{kpi.unit}</span>
        )}
      </div>
      {kpi.hint && (
        <p className="mt-1 line-clamp-2 text-[11px] leading-snug text-ink-faint">
          {kpi.hint}
        </p>
      )}
    </div>
  );
}

// ─── Bespoke table — scannable, mono+linked contract ids, right-aligned nums ─

export function BespokeTablePanel({
  table,
  source,
}: {
  table: BespokeTable;
  source: RequestExample;
}) {
  // Decide alignment per column: right-align a column if every non-empty cell
  // in it looks numeric (so $-formatted / count columns sit flush-right).
  const numericCols = table.columns.map((_, ci) =>
    table.rows.every((r) => {
      const cell = r[ci];
      return cell == null || cell === '' || looksNumeric(cell);
    }),
  );

  return (
    <Panel title={table.title} source={source} bodyClassName="-mx-4">
      <div className="overflow-x-auto">
        <table className="min-w-full divide-y divide-line text-sm">
          <thead>
            <tr className="text-left text-[10px] uppercase tracking-wider text-ink-muted">
              {table.columns.map((c, ci) => (
                <th
                  key={ci}
                  scope="col"
                  className={`px-4 py-2 ${numericCols[ci] ? 'text-right' : ''}`}
                >
                  {c}
                </th>
              ))}
            </tr>
          </thead>
          <tbody className="divide-y divide-line-subtle">
            {table.rows.length === 0 ? (
              <tr>
                <td
                  colSpan={table.columns.length}
                  className="px-4 py-6 text-center text-sm text-ink-muted"
                >
                  No rows in the window.
                </td>
              </tr>
            ) : (
              table.rows.map((row, ri) => (
                <tr
                  key={ri}
                  className="hover:bg-surface-muted"
                >
                  {table.columns.map((_, ci) => (
                    <td
                      key={ci}
                      className={`px-4 py-2 ${
                        numericCols[ci]
                          ? 'text-right font-mono tabular-nums text-ink-body'
                          : ''
                      }`}
                    >
                      <Cell value={row[ci] ?? ''} />
                    </td>
                  ))}
                </tr>
              ))
            )}
          </tbody>
        </table>
      </div>
    </Panel>
  );
}

// Cell — renders entity ids as mono, copyable, click-through links into the
// explorer (contracts → /contract, accounts → /accounts); shortens any other
// long id (e.g. a CODE-ISSUER canonical asset) copyably; everything else plain.
// The store ships RAW ids — all shortening/linking is presentation, done here.
function Cell({ value }: { value: string }) {
  if (value === '' || value === '—') {
    return <span className="text-ink-faint">—</span>;
  }
  if (isContractId(value)) {
    return (
      <Link
        href={`/contracts/${encodeURIComponent(value)}/`}
        className="text-brand-600 hover:underline"
      >
        <CopyHash value={value} head={8} tail={6} />
      </Link>
    );
  }
  if (isAccountId(value)) {
    return (
      <Link
        href={`/accounts/${encodeURIComponent(value)}/`}
        className="text-brand-600 hover:underline"
      >
        <CopyHash value={value} head={8} tail={6} />
      </Link>
    );
  }
  if (isTxHash(value)) {
    return (
      <Link
        href={`/transactions/${encodeURIComponent(value)}/`}
        className="text-brand-600 hover:underline"
      >
        <CopyHash value={value} head={8} tail={6} />
      </Link>
    );
  }
  // Long non-strkey id (e.g. "USDC-G…" canonical asset) — shorten + copy, no
  // link (no asset-by-id route). Short labels ("native", codes) pass through.
  if (value.length > 28) {
    return <CopyHash value={value} head={10} tail={6} />;
  }
  return <span>{value}</span>;
}

// ─── helpers ─────────────────────────────────────────────────────────────

// toChartNumber — parse a bespoke series string to a JS number for chart
// geometry only (values can exceed 2^53 — ADR-0003; never use the result as
// a served amount). Strips $ / , / % decoration the server may include so
// the shape still plots. Returns null — callers DROP the point — for a
// non-numeric value (a label, an em-dash: plotting it as 0 fabricated data)
// and for compact-suffixed figures ("1.2M"): stripping the suffix mis-scaled
// them 10^6 off, and a silently-wrong point is worse than a missing one.
// Exported for BridgeShowcase, which shares the wire shape.
export function toChartNumber(v: string): number | null {
  if (!v) return null;
  const cleaned = v.replace(/[$,%\s]/g, '');
  if (cleaned === '' || /[a-zA-Z]/.test(cleaned)) return null;
  const n = Number(cleaned);
  return Number.isFinite(n) ? n : null;
}

// isContractId — a Soroban C-strkey: starts with 'C', 56 chars, base32 body.
function isContractId(v: string): boolean {
  return v.length === 56 && v[0] === 'C' && /^[A-Z2-7]+$/.test(v);
}

// isAccountId — a Stellar G-strkey account address: 'G', 56 chars, base32.
function isAccountId(v: string): boolean {
  return v.length === 56 && v[0] === 'G' && /^[A-Z2-7]+$/.test(v);
}

// isTxHash — a Stellar transaction hash: 64 lowercase hex chars.
function isTxHash(v: string): boolean {
  return v.length === 64 && /^[0-9a-f]+$/.test(v);
}

// looksNumeric — a cell is "numeric" for alignment if, once common money/count
// decoration is stripped, it parses as a finite number. Keeps "$1.2M", "1,234",
// "12.5%", "—" all classed sensibly.
function looksNumeric(v: string): boolean {
  if (v === '—') return true;
  const cleaned = v.replace(/[$,%\s]/g, '');
  if (cleaned === '') return false;
  // Allow a compact-suffix figure ("1.2M", "3.4K") to count as numeric too.
  const compact = cleaned.replace(/[KMBTkmbt]$/, '');
  return Number.isFinite(Number(compact));
}

// Re-export so the host page can format the at-a-glance line consistently.
export { formatCompact };
