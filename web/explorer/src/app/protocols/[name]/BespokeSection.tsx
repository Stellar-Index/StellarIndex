'use client';

import { useMemo, useState } from 'react';
import dynamic from 'next/dynamic';
import Link from 'next/link';
import { useQuery } from '@tanstack/react-query';

import { Panel } from '@/components/reveal';
import { apiGet, type RequestExample } from '@/api/client';
import {
  formatCompact,
  formatDecimalAmount,
  formatReadable,
  sumDecimalStrings,
} from '@/lib/format';
import { CATEGORICAL_PALETTE } from '@/components/charts/DonutChart';
import { dropPartialTrailingDay, seriesPointTime } from '@/lib/series';
import { CopyHash } from '../../explorer-shared';
import { AssetText } from '@/components/AssetLink';
import { TimeSeriesChart } from './TimeSeriesChart';
import {
  Button,
  Segmented,
  Skeleton,
  Stat,
  StatCell,
  StatGrid,
  TBody,
  TR,
  Table,
  Td,
  Th,
  THead,
} from '@/components/ui';
import { PANEL_LABEL_CLASS, SECTION_HEADING_CLASS } from '@/components/ui/Page';
import { categoryLabel } from '../registry';
import {
  BridgeShowcase,
  BreakdownDonuts,
  LineLegend,
  type LegendLine,
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

// ─── Section-level analytics window ──────────────────────────────────────
//
// The 24h/7d/30d/90d pills live at BespokeSection level (not in the
// bridge showcase) so EVERY category's whole bespoke block —
// KPIs, series, donuts, tables — is window-reactive. Switching a pill
// refetches `/v1/protocols/{name}?days=N`; the server keys its detail
// cache on (name, days) and buckets the 24h window hourly.

const WINDOWS = [
  { key: '24h', days: 1 },
  { key: '7d', days: 7 },
  { key: '30d', days: 30 },
  { key: '90d', days: 90 },
] as const;

export type WindowDays = (typeof WINDOWS)[number]['days'];

/** The page's initial fetch covers the server default window (90d). */
const DEFAULT_WINDOW_DAYS: WindowDays = 90;

export function windowLabelFor(days: WindowDays): string {
  return WINDOWS.find((w) => w.days === days)?.key ?? `${days}d`;
}

/** The shared 24h/7d/30d/90d pill row — ui/Segmented (role, aria-label and
 *  focus ring come from Segmented). */
function WindowPills({
  days,
  onChange,
}: {
  days: WindowDays;
  onChange: (d: WindowDays) => void;
}) {
  return (
    <Segmented
      ariaLabel="Analytics window"
      options={WINDOWS.map((w) => ({ label: w.key, value: w.key }))}
      value={windowLabelFor(days)}
      onChange={(key) => {
        const next = WINDOWS.find((w) => w.key === key);
        if (next) onChange(next.days);
      }}
    />
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
  const lines: LegendLine[] = group.series.map((s, i) => ({
    label: s.name.slice(group.title.length + ' · '.length),
    tone: 'brand',
    color: CATEGORICAL_PALETTE[i % CATEGORICAL_PALETTE.length],
    total: seriesTotal(s.points),
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
      hint={`${unit ? `${unit} · ` : ''}${grainLabel} · last ${windowLabelFor(days)}`}
      source={source}
    >
      <div className="space-y-3">
        <LineLegend lines={lines} />
        <LineChart
          data={[]}
          series={lines}
          height={224}
          priceFormat={{ type: 'volume', precision: 0 }}
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
 * BespokeSection — the category-specific analytics block. Renders ONLY the
 * sub-parts the server populated; the whole section is omitted upstream
 * when `data.bespoke` is absent. A flat section heading with the window
 * pills, a StatGrid of KPIs, then one Panel per series, table or breakdown:
 * the same chrome as the activity panels around it.
 *
 * The section OWNS the analytics window: the 24h/7d/30d/90d pills in the
 * header refetch `/v1/protocols/{name}?days=N` and swap the WHOLE block
 * (KPIs included — the server labels them "(7d)" etc. per window). The
 * default window reuses the block the page already fetched — no duplicate
 * request on first render. Bridge blocks delegate their chart suite to
 * BridgeShowcase, which consumes this section-level window instead of
 * owning pills; its all-time cumulative headline stays
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
  const { data, isFetching, isError, refetch } = useQuery<Bespoke | null>({
    queryKey: ['/v1/protocols/{name}', name, days, 'bespoke-window'],
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
    <section aria-labelledby="bespoke-heading" className="space-y-4">
      <div className="flex flex-wrap items-center gap-x-4 gap-y-2">
        <div>
          <h2 id="bespoke-heading" className={SECTION_HEADING_CLASS}>
            {categoryLabel(bespoke.category)} analytics
          </h2>
          <p className="text-ink-muted mt-0.5 text-sm">
            Tailored on-chain metrics · the window applies to every panel below
          </p>
        </div>
        <div className="ml-auto">
          <WindowPills days={days} onChange={setDays} />
        </div>
      </div>

      {loading && hasKpis ? (
        <Skeleton
          aria-hidden
          className="h-24 w-full"
          data-testid="bespoke-kpis-loading"
        />
      ) : (
        activeKpis.length > 0 && (
          <StatGrid cols={4}>
            {activeKpis.map((k, i) => {
              const shown = kpiDisplay(k);
              return (
                <StatCell key={`${k.label}-${i}`}>
                  <Stat
                    label={<span title={k.hint || undefined}>{k.label}</span>}
                    value={<span title={k.value}>{shown.value}</span>}
                    sub={shown.unit}
                  />
                </StatCell>
              );
            })}
          </StatGrid>
        )
      )}

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
          onRetry={() => refetch()}
          source={source}
        />
      ) : failed ? (
        <WindowFailed onRetry={() => refetch()} />
      ) : loading ? (
        <Skeleton
          aria-hidden
          className="h-56 w-full"
          data-testid="bespoke-window-loading"
        />
      ) : active === null ? (
        <p className="text-ink-muted py-6 text-center text-sm">
          No analytics in this window.
        </p>
      ) : (
        <>
          {standalone.map((s, i) => (
            <Panel
              key={`${s.name}-${i}`}
              title={s.name}
              hint={`${s.unit ? `${s.unit} · ` : ''}${days === 1 ? 'hourly' : 'daily'} · last ${windowLabelFor(days)}`}
              source={source}
            >
              <TimeSeriesChart
                points={dropPartialTrailingDay(s.points).flatMap((p) => {
                  const value = toChartNumber(p.value);
                  return value == null ? [] : [{ date: p.date, value }];
                })}
                label={s.name}
                unit={s.unit}
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

      {/* Caveats belong to the active window's block and stay visible. */}
      {notes.length > 0 && (
        <div>
          <div className={PANEL_LABEL_CLASS}>Notes</div>
          <ul className="text-ink-muted mt-2 space-y-1 text-xs leading-relaxed">
            {notes.map((n, i) => (
              <li key={i}>{n}</li>
            ))}
          </ul>
        </div>
      )}
    </section>
  );
}

/** The failed-window state, shared with the bridge showcase. */
export function WindowFailed({ onRetry }: { onRetry?: () => void }) {
  return (
    <div className="py-6 text-center">
      <p className="text-ink-muted text-sm">Couldn&apos;t load this window.</p>
      {onRetry && (
        <Button
          variant="secondary"
          size="sm"
          className="mt-3"
          onClick={onRetry}
        >
          Retry
        </Button>
      )}
    </div>
  );
}

// ─── KPI display ─────────────────────────────────────────────────────────

// Values arrive as raw decimal strings; group them as strings (ADR-0003) and
// render USD as a dollar figure. Non-decimal values pass through untouched.
function kpiDisplay(kpi: BespokeKpi): { value: string; unit?: string } {
  if (kpi.unit === 'USD') {
    const v = formatReadable(kpi.value, true);
    if (v != null) return { value: v };
  }
  if (!IDENTIFIER_LABEL.test(kpi.label)) {
    const v = formatReadable(kpi.value);
    if (v != null) return { value: v, unit: kpi.unit };
  }
  const frac = kpi.value.split('.')[1]?.length ?? 0;
  return {
    value: formatDecimalAmount(kpi.value, frac) ?? kpi.value,
    unit: kpi.unit,
  };
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
    <Panel title={table.title} source={source} bodyClassName="-mx-4 -mb-4">
      <div className="overflow-x-auto">
        <Table>
          <THead>
            <tr>
              {table.columns.map((c, ci) => (
                <Th key={ci} align={numericCols[ci] ? 'right' : undefined}>
                  {c}
                </Th>
              ))}
            </tr>
          </THead>
          <TBody>
            {table.rows.length === 0 ? (
              <TR>
                <Td colSpan={table.columns.length} className="text-ink-muted">
                  No rows in the window.
                </Td>
              </TR>
            ) : (
              table.rows.map((row, ri) => (
                <TR key={ri}>
                  {table.columns.map((_, ci) => (
                    <Td
                      key={ci}
                      align={numericCols[ci] ? 'right' : undefined}
                      className={numericCols[ci] ? 'font-mono' : undefined}
                    >
                      <Cell
                        value={row[ci] ?? ''}
                        column={table.columns[ci] ?? ''}
                      />
                    </Td>
                  ))}
                </TR>
              ))
            )}
          </TBody>
        </Table>
      </div>
    </Panel>
  );
}

// Cell — renders entity ids as mono, copyable, click-through links into the
// explorer (contracts → /contract, accounts → /accounts); shortens any other
// long id (e.g. a CODE-ISSUER canonical asset) copyably; everything else plain.
// The store ships RAW ids — all shortening/linking is presentation, done here.
function Cell({ value, column }: { value: string; column: string }) {
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
  if (value === 'native' || CLASSIC_ASSET_ID.test(value)) {
    return <AssetText canonical={value} />;
  }
  const readable = IDENTIFIER_LABEL.test(column)
    ? null
    : formatReadable(value, /usd/i.test(column));
  if (readable != null) return <span title={value}>{readable}</span>;
  if (value.length > 28) {
    return <CopyHash value={value} head={10} tail={6} />;
  }
  return <span>{value}</span>;
}

// ─── helpers ─────────────────────────────────────────────────────────────

// Columns and KPIs whose numbers are identifiers, shown verbatim.
const IDENTIFIER_LABEL = /ledger|seq|\bid\b|hash|year|nonce|version/i;

const CLASSIC_ASSET_ID = /^[A-Za-z0-9]{1,12}-G[A-Z2-7]{55}$/;

// toChartNumber — parse a bespoke series string to a JS number for chart
// geometry only (values can exceed 2^53 — ADR-0003; never use the result as
// a served amount). Strips $ / , / % decoration the server may include so
// the shape still plots. Returns null — callers DROP the point — for a
// non-numeric value (a label, an em-dash: plotting it as 0 fabricated data)
// and for compact-suffixed figures ("1.2M"): stripping the suffix mis-scaled
// them 10^6 off, and a silently-wrong point is worse than a missing one.
// Exported for BridgeShowcase, which shares the wire shape.
export function toChartNumber(v: string): number | null {
  const cleaned = cleanBespokeValue(v);
  if (cleaned == null) return null;
  const n = Number(cleaned);
  return Number.isFinite(n) ? n : null;
}

// toDecimalString — toChartNumber's parse kept as an exact decimal string,
// for totals summed with sumDecimalStrings rather than in float.
export function toDecimalString(v: string): string | null {
  const cleaned = cleanBespokeValue(v);
  return cleaned != null && /^-?\d+(\.\d+)?$/.test(cleaned) ? cleaned : null;
}

// seriesTotal — the exact sum of the points a line plots.
export function seriesTotal(points: BespokeSeries['points']): string | null {
  return sumDecimalStrings(
    dropPartialTrailingDay(points)
      .filter((p) => seriesPointTime(p.date) != null)
      .map((p) => toDecimalString(p.value)),
  );
}

function cleanBespokeValue(v: string): string | null {
  if (!v) return null;
  const cleaned = v.replace(/[$,%\s]/g, '');
  if (cleaned === '' || /[a-zA-Z]/.test(cleaned)) return null;
  return cleaned;
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
