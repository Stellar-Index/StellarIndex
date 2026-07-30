'use client';

import { useMemo } from 'react';
import dynamic from 'next/dynamic';

import { Panel } from '@/components/reveal';
import type { RequestExample } from '@/api/client';
import { cn } from '@/lib/cn';
import { formatCompact } from '@/lib/format';
import type { NamedLineSeries, LineSeriesTone } from '@/components/charts/LineChart';
import { DonutChart, CATEGORICAL_PALETTE, type DonutSlice } from '@/components/charts/DonutChart';
import {
  toChartNumber,
  BespokeTablePanel,
  windowLabelFor,
  type Bespoke,
  type BespokeSeries,
  type BespokeBreakdown,
  type WindowDays,
} from './BespokeSection';

// BridgeShowcase — the bridge protocols' window-reactive analytics suite.
// For cctp it renders the full SDF-showcase: the all-time cumulative
// net-inflow headline chart, the combined inbound/outbound flow chart,
// side-by-side source/destination donuts, per-chain top-5 multi-line charts,
// and the largest-transfers table. The 24h/7d/30d/90d window pills and the
// `?days=N` refetch live at BespokeSection level (lifted 2026-07-30 so
// every category is window-reactive); this component consumes the section's
// window + fetch state as props. For rozo (a single settled series, no
// breakdowns) it degrades to one flow line.
//
// The server buckets the 24h window HOURLY (daily otherwise) and keeps
// series names window-stable so all pairing below is by exact name/prefix:
//   "Inbound (USDC)" / "Outbound (USDC)"   — the flow totals
//   "Inbound · <chain>" / "Outbound · <chain>" — per-chain top-5
//   "Cumulative net inflow (all-time)"     — window-independent headline
//
// Values arrive as numeric STRINGS that can exceed 2^53 (ADR-0003):
// Number() is used ONLY for pixel geometry; displayed figures go through
// formatCompact or are server-formatted strings.

const LineChart = dynamic(
  () => import('@/components/charts/LineChart').then((m) => m.LineChart),
  { ssr: false, loading: () => <div className="h-56 w-full" /> },
);

// Server-stable series names (mirror internal/storage/timescale
// protocol_bespoke_cctp.go's cctpSeriesName* constants).
const INBOUND_TOTAL = 'Inbound (USDC)';
const OUTBOUND_TOTAL = 'Outbound (USDC)';
const PER_CHAIN_IN = 'Inbound · ';
const PER_CHAIN_OUT = 'Outbound · ';
const CUMULATIVE = 'Cumulative net inflow (all-time)';

// pointTime — bespoke point date → unix seconds. Daily points are
// "YYYY-MM-DD"; hourly points (the 24h window) are "YYYY-MM-DDTHH:MM".
// Exported for BespokeSection's grouped multi-line panels.
export function pointTime(date: string): number {
  const iso =
    date.length === 10 ? `${date}T00:00:00Z` : date.length === 16 ? `${date}:00Z` : date;
  const ms = Date.parse(iso);
  return Number.isFinite(ms) ? Math.floor(ms / 1000) : 0;
}

function toLine(
  s: BespokeSeries,
  label: string,
  tone: LineSeriesTone,
  color?: string,
): NamedLineSeries {
  return {
    label,
    tone,
    color,
    data: s.points.map((p) => ({ time: pointTime(p.date), value: toChartNumber(p.value) })),
  };
}

// isAuxSeries — per-chain and cumulative series are auxiliary to the flow
// totals chart and must never be picked as its single-series fallback.
function isAuxSeries(s: BespokeSeries): boolean {
  return (
    s.name.startsWith(PER_CHAIN_IN) ||
    s.name.startsWith(PER_CHAIN_OUT) ||
    s.name === CUMULATIVE
  );
}

// flowLines — project a bespoke series list onto the totals chart's named
// lines: the inbound/outbound pair when both are present (cctp), else the
// first non-auxiliary series as a single line (rozo's settled volume).
export function flowLines(series: BespokeSeries[]): NamedLineSeries[] {
  const inbound = series.find((s) => s.name === INBOUND_TOTAL);
  const outbound = series.find((s) => s.name === OUTBOUND_TOTAL);
  if (inbound && outbound) {
    return [toLine(inbound, inbound.name, 'up'), toLine(outbound, outbound.name, 'brand')];
  }
  const single = series.find((s) => !isAuxSeries(s));
  return single ? [toLine(single, single.name, 'brand')] : [];
}

// ─── Stable chain → hue assignment ───────────────────────────────────────
//
// Color follows the ENTITY, never its rank: a chain keeps one hue across
// window switches, both donuts and both per-chain line charts. Chains
// outside the fixed order (plus honest-unknown labels and the "Others"
// bucket) fold onto the neutral tail slot.
const CHAIN_ORDER = [
  'Base',
  'Ethereum',
  'Solana',
  'Avalanche',
  'Arbitrum',
  'Polygon PoS',
  'OP Mainnet',
  'Sonic',
  'Noble',
];

export function chainColor(label: string): string {
  const i = CHAIN_ORDER.indexOf(label);
  const tail = CATEGORICAL_PALETTE[CATEGORICAL_PALETTE.length - 1];
  if (i < 0) return tail;
  return CATEGORICAL_PALETTE[i % (CATEGORICAL_PALETTE.length - 1)];
}

// perChainLines — the "Inbound · <chain>" / "Outbound · <chain>" series
// (already top-5, volume-descending from the server) as palette-colored
// lines labelled by bare chain name.
export function perChainLines(series: BespokeSeries[], prefix: string): NamedLineSeries[] {
  return series
    .filter((s) => s.name.startsWith(prefix))
    .map((s) => {
      const chain = s.name.slice(prefix.length);
      return toLine(s, chain, 'brand', chainColor(chain));
    });
}

// donutSlices — breakdown rows → top-6 slices + an "Others" fold (values
// via Number() for geometry only).
export function donutSlices(b: BespokeBreakdown): DonutSlice[] {
  const rows = b.rows
    .map((r) => ({ label: r.label, value: toChartNumber(r.value) }))
    .filter((r) => Number.isFinite(r.value) && r.value > 0)
    .sort((a, x) => x.value - a.value);
  const top = rows.slice(0, 6).map((r) => ({ ...r, color: chainColor(r.label) }));
  const rest = rows.slice(6);
  if (rest.length > 0) {
    top.push({
      label: `Others (${rest.length})`,
      value: rest.reduce((sum, r) => sum + r.value, 0),
      color: CATEGORICAL_PALETTE[CATEGORICAL_PALETTE.length - 1],
    });
  }
  return top;
}

// donutHeading — friendly SDF-facing headline per breakdown direction; the
// server's data-honest title becomes the hint.
function donutHeading(title: string): string {
  if (/inflow|source/i.test(title)) return 'Where funds come from';
  if (/outflow|destination/i.test(title)) return 'Where funds go';
  return title;
}

/**
 * BreakdownDonuts — side-by-side composition donuts from the generic
 * bespoke `breakdowns` wire field. Exported for non-bridge categories that
 * adopt breakdowns (BespokeSection renders it generically there).
 */
export function BreakdownDonuts({
  breakdowns,
  source,
  windowLabel,
}: {
  breakdowns: BespokeBreakdown[];
  source: RequestExample;
  windowLabel?: string;
}) {
  if (breakdowns.length === 0) return null;
  return (
    <div className="grid gap-4 lg:grid-cols-2">
      {breakdowns.map((b) => {
        const slices = donutSlices(b);
        return (
          <Panel
            key={b.title}
            title={donutHeading(b.title)}
            hint={`${b.title}${b.unit ? ` · ${b.unit}` : ''}${windowLabel ? ` · last ${windowLabel}` : ''}`}
            source={source}
          >
            <DonutChart
              data={slices}
              centerLabel={formatCompact(slices.reduce((s, x) => s + x.value, 0))}
              centerSub={b.unit || undefined}
              formatValue={formatCompact}
            />
          </Panel>
        );
      })}
    </div>
  );
}

// LineLegend — the static legend row for a multi-line chart: one entry per
// line with its hue swatch and window total. Exported for BespokeSection's
// grouped multi-line panels.
export function LineLegend({ lines }: { lines: NamedLineSeries[] }) {
  if (lines.length === 0) return null;
  return (
    <ul className="flex flex-wrap gap-x-4 gap-y-1 text-xs text-ink-muted">
      {lines.map((l) => (
        <li key={l.label} className="flex items-center gap-1.5">
          <span
            aria-hidden
            className={cn(
              'inline-block h-2 w-2 rounded-full',
              !l.color && (l.tone === 'up' ? 'bg-up' : l.tone === 'down' ? 'bg-down' : 'bg-brand-500'),
            )}
            style={l.color ? { backgroundColor: l.color } : undefined}
          />
          <span>{l.label}</span>
          <span className="font-mono tabular-nums text-ink-body">
            {formatCompact(l.data.reduce((s, p) => s + p.value, 0))}
          </span>
        </li>
      ))}
    </ul>
  );
}

export function BridgeShowcase({
  initial,
  active,
  days,
  isFetching,
  isError,
  source,
}: {
  /** The bespoke block from the page's initial (90d) fetch. */
  initial: Bespoke;
  /**
   * The active window's block from BespokeSection (null while the window
   * refetch is in flight or failed).
   */
  active: Bespoke | null;
  /** The section-level analytics window driving every chart below. */
  days: WindowDays;
  isFetching: boolean;
  isError: boolean;
  source: RequestExample;
}) {
  const series = useMemo(() => active?.series ?? [], [active]);

  const lines = useMemo(() => flowLines(series), [series]);
  const inboundChainLines = useMemo(() => perChainLines(series, PER_CHAIN_IN), [series]);
  const outboundChainLines = useMemo(() => perChainLines(series, PER_CHAIN_OUT), [series]);
  // The cumulative series is all-time and window-independent — always
  // render it from the page's initial fetch so pill switches never blank
  // the headline chart.
  const cumulative = useMemo(
    () => (initial.series ?? []).find((s) => s.name === CUMULATIVE),
    [initial.series],
  );
  const breakdowns = active?.breakdowns ?? [];
  const tables = active?.tables ?? [];

  const dual = lines.length > 1;
  const unit = series.find((s) => !isAuxSeries(s))?.unit;
  const hasPoints = lines.some((l) => l.data.length > 0);
  const windowLabel = windowLabelFor(days);
  const grainLabel = days === 1 ? 'hourly' : 'daily';

  return (
    <div className="space-y-4">
      {/* ── The SDF headline: all-time cumulative net inflow ── */}
      {cumulative && cumulative.points.length > 0 && (
        <Panel
          title="Cumulative net inflow"
          hint={`all-time · daily · inbound − outbound${cumulative.unit ? ` · ${cumulative.unit}` : ''}`}
          source={source}
        >
          <LineChart
            data={cumulative.points.map((p) => ({
              time: pointTime(p.date),
              value: toChartNumber(p.value),
            }))}
            height={224}
            legend={{ valueLabel: 'Net inflow', formatValue: formatCompact }}
            ariaLabel={`Cumulative net USDC inflow onto Stellar since the bridge launched${cumulative.unit ? `, in ${cumulative.unit}` : ''}.`}
          />
        </Panel>
      )}

      {/* ── Flow totals (driven by the section-level window pills) ── */}
      <Panel
        title={dual ? 'Bridge flows' : (lines[0]?.label ?? 'Bridge flows')}
        hint={`${unit ? `Units: ${unit} · ` : ''}${grainLabel} buckets · last ${windowLabel}${dual ? ' · window applies to the charts and table below' : ''}`}
        source={source}
      >
        <div className="space-y-3">
          {hasPoints && <LineLegend lines={lines} />}

          {isError ? (
            <p className="py-6 text-center text-sm text-ink-muted">
              Couldn&apos;t load this window — retry, or pick another window.
            </p>
          ) : isFetching ? (
            <div
              aria-hidden
              className="h-56 w-full animate-pulse rounded-md bg-surface-subtle"
              data-testid="bridge-flows-loading"
            />
          ) : !hasPoints ? (
            <p className="py-6 text-center text-sm text-ink-muted">
              No transfers in this window.
            </p>
          ) : (
            <LineChart
              data={[]}
              series={lines}
              height={224}
              timeVisible={days === 1}
              legend={{ valueLabel: lines[0]?.label ?? '', formatValue: formatCompact }}
              ariaLabel={`${dual ? 'Inbound and outbound bridge flow' : (lines[0]?.label ?? 'Flow')} over the last ${windowLabel}${unit ? `, in ${unit}` : ''}.`}
            />
          )}
        </div>
      </Panel>

      {/* ── Where funds come from / go (window-reactive donuts) ── */}
      {breakdowns.length > 0 && (
        <BreakdownDonuts breakdowns={breakdowns} source={source} windowLabel={windowLabel} />
      )}

      {/* ── Per-chain top-5 lines, both directions ── */}
      {(inboundChainLines.length > 0 || outboundChainLines.length > 0) && (
        <div className="grid gap-4 xl:grid-cols-2">
          {inboundChainLines.length > 0 && (
            <Panel
              title="Inflows by source chain"
              hint={`top ${inboundChainLines.length} chains by volume · ${grainLabel} · last ${windowLabel}`}
              source={source}
            >
              <div className="space-y-3">
                <LineLegend lines={inboundChainLines} />
                <LineChart
                  data={[]}
                  series={inboundChainLines}
                  height={200}
                  timeVisible={days === 1}
                  legend={{ valueLabel: inboundChainLines[0]?.label ?? '', formatValue: formatCompact }}
                  ariaLabel={`Inbound USDC per source chain over the last ${windowLabel}.`}
                />
              </div>
            </Panel>
          )}
          {outboundChainLines.length > 0 && (
            <Panel
              title="Outflows by destination chain"
              hint={`top ${outboundChainLines.length} chains by volume · ${grainLabel} · last ${windowLabel}`}
              source={source}
            >
              <div className="space-y-3">
                <LineLegend lines={outboundChainLines} />
                <LineChart
                  data={[]}
                  series={outboundChainLines}
                  height={200}
                  timeVisible={days === 1}
                  legend={{ valueLabel: outboundChainLines[0]?.label ?? '', formatValue: formatCompact }}
                  ariaLabel={`Outbound USDC per destination chain over the last ${windowLabel}.`}
                />
              </div>
            </Panel>
          )}
        </div>
      )}

      {/* ── Largest transfers (window-reactive) ── */}
      {tables.map((t) => (
        <BespokeTablePanel key={t.title} table={t} source={source} />
      ))}
    </div>
  );
}
