'use client';

import { useMemo } from 'react';

import { formatCompact } from '@/lib/format';

// TimeSeriesChart — a hand-rolled, dependency-free SVG area chart shared
// by the per-protocol page: the always-present on-chain activity series and
// every bespoke `series` entry render through this one component (DRY — the
// geometry used to live inline in ProtocolView's ActivityChart).
//
// Inputs are (date, value) points where `value` is a JS number used ONLY for
// chart geometry. The bespoke wire ships values as numeric STRINGS that can
// exceed 2^53 (ADR-0003); the caller parses them with Number() for the shape
// and shows the formatCompact-formatted figure in the axis/peak labels — the
// precision loss is cosmetic (a pixel y-coordinate), never a served amount.

export interface ChartPoint {
  date: string; // YYYY-MM-DD
  value: number;
}

// Chart geometry — module-level constants so the useMemo dep arrays can
// reference them without ESLint flagging a recreated object literal each render.
const CHART_W = 1000;
const CHART_H = 240;
const CHART_PAD = { top: 12, right: 8, bottom: 22, left: 8 };

const TONES = {
  emerald: { line: 'rgb(5 150 105)', fill: 'rgb(16 185 129)' },
  brand: { line: 'rgb(2 132 199)', fill: 'rgb(14 165 233)' },
  violet: { line: 'rgb(124 58 237)', fill: 'rgb(139 92 246)' },
  amber: { line: 'rgb(217 119 6)', fill: 'rgb(245 158 11)' },
  indigo: { line: 'rgb(79 70 229)', fill: 'rgb(99 102 241)' },
} as const;

export type ChartTone = keyof typeof TONES;

export function TimeSeriesChart({
  points,
  label,
  unit,
  tone = 'emerald',
  gradientId,
}: {
  /** The series to plot; `value` is already a JS number (geometry only). */
  points: ChartPoint[];
  /** Human label for the aria summary (e.g. "USD volume", "daily events"). */
  label: string;
  /** Optional unit appended to the peak/avg figures (e.g. "USD", "events"). */
  unit?: string;
  /** Palette key — lets multiple charts on one page read distinctly. */
  tone?: ChartTone;
  /** Unique <linearGradient> id (SVG ids are document-global — must differ). */
  gradientId: string;
}) {
  const geom = useMemo(() => {
    const pad = CHART_PAD;
    const n = points.length;
    const max = points.reduce((m, p) => Math.max(m, p.value), 0);
    const innerW = CHART_W - pad.left - pad.right;
    const innerH = CHART_H - pad.top - pad.bottom;
    const x = (i: number) =>
      pad.left + (n <= 1 ? innerW / 2 : (i / (n - 1)) * innerW);
    const y = (v: number) =>
      pad.top + innerH - (max <= 0 ? 0 : (v / max) * innerH);

    const linePts = points.map((p, i) => `${x(i)},${y(p.value)}`).join(' ');
    const areaPath =
      n === 0
        ? ''
        : `M ${x(0)},${pad.top + innerH} ` +
          points.map((p, i) => `L ${x(i)},${y(p.value)}`).join(' ') +
          ` L ${x(n - 1)},${pad.top + innerH} Z`;

    const total = points.reduce((s, p) => s + p.value, 0);
    const avg = n > 0 ? total / n : 0;
    return { max, x, y, linePts, areaPath, total, avg, innerH };
  }, [points]);

  // Sparse x-axis ticks: first, middle, last dates.
  const ticks = useMemo(() => {
    if (points.length === 0) return [] as { i: number; label: string }[];
    const idxs = Array.from(
      new Set([0, Math.floor((points.length - 1) / 2), points.length - 1]),
    );
    return idxs.map((i) => ({ i, label: shortDate(points[i].date) }));
  }, [points]);

  if (points.length === 0) {
    return (
      <p className="py-6 text-center text-sm text-ink-muted">
        No data points in the window.
      </p>
    );
  }

  const colour = TONES[tone] ?? TONES.emerald;
  const unitSuffix = unit ? ` ${unit}` : '';
  const peak = points.reduce(
    (best, p) => (p.value > best.value ? p : best),
    points[0],
  );
  const ariaLabel = `${label}: ${points.length} points, peak ${formatCompact(
    peak.value,
  )}${unitSuffix} on ${peak.date}, average ${formatCompact(
    Math.round(geom.avg),
  )}${unitSuffix} over the window.`;

  return (
    <div className="space-y-2">
      <div className="flex flex-wrap items-baseline gap-x-6 gap-y-1 text-xs text-ink-muted">
        <span>
          Peak{' '}
          <span className="font-mono tabular-nums text-ink-body">
            {formatCompact(peak.value)}
            {unitSuffix}
          </span>{' '}
          on {peak.date}
        </span>
        <span>
          Avg/point{' '}
          <span className="font-mono tabular-nums text-ink-body">
            {formatCompact(Math.round(geom.avg))}
            {unitSuffix}
          </span>
        </span>
        <span>
          Latest{' '}
          <span className="font-mono tabular-nums text-ink-body">
            {formatCompact(points[points.length - 1].value)}
            {unitSuffix}
          </span>
        </span>
      </div>
      <svg
        viewBox={`0 0 ${CHART_W} ${CHART_H}`}
        preserveAspectRatio="none"
        className="h-56 w-full"
        role="img"
        aria-label={ariaLabel}
      >
        <defs>
          <linearGradient id={gradientId} x1="0" y1="0" x2="0" y2="1">
            <stop offset="0%" stopColor={colour.fill} stopOpacity="0.28" />
            <stop offset="100%" stopColor={colour.fill} stopOpacity="0" />
          </linearGradient>
        </defs>
        {/* baseline */}
        <line
          x1={CHART_PAD.left}
          y1={CHART_PAD.top + geom.innerH}
          x2={CHART_W - CHART_PAD.right}
          y2={CHART_PAD.top + geom.innerH}
          stroke="rgb(148 163 184 / 0.3)"
          strokeWidth={1}
        />
        {geom.areaPath && (
          <path d={geom.areaPath} fill={`url(#${gradientId})`} />
        )}
        {geom.linePts && (
          <polyline
            points={geom.linePts}
            fill="none"
            stroke={colour.line}
            strokeWidth={2}
            strokeLinejoin="round"
            strokeLinecap="round"
            vectorEffect="non-scaling-stroke"
          />
        )}
        {ticks.map((t) => (
          <text
            key={t.i}
            x={geom.x(t.i)}
            y={CHART_H - 6}
            textAnchor={
              t.i === 0
                ? 'start'
                : t.i === points.length - 1
                  ? 'end'
                  : 'middle'
            }
            className="fill-ink-faint"
            style={{ fontSize: 11, fontFamily: 'var(--font-sans)' }}
          >
            {t.label}
          </text>
        ))}
      </svg>
    </div>
  );
}

// shortDate — YYYY-MM-DD → "MMM D" (UTC, no Date parse ambiguity). Shared
// by both the activity chart and the bespoke charts.
export function shortDate(iso: string): string {
  const m = /^(\d{4})-(\d{2})-(\d{2})$/.exec(iso);
  if (!m) return iso;
  const months = [
    'Jan', 'Feb', 'Mar', 'Apr', 'May', 'Jun',
    'Jul', 'Aug', 'Sep', 'Oct', 'Nov', 'Dec',
  ];
  const mon = months[Number(m[2]) - 1] ?? m[2];
  return `${mon} ${Number(m[3])}`;
}
