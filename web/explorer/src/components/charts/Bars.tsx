'use client';

import { cn } from '@/lib/cn';

// Shared horizontal-bar primitives for categorical magnitude comparisons —
// the "bar" complement to DonutChart (composition) and LineChart (time).
// Pure HTML/CSS marks (no canvas) so they stay crisp, printable, and
// testable; colors come from the design tokens / CATEGORICAL_PALETTE at the
// call site. Values are JS numbers for GEOMETRY ONLY — callers pass a
// pre-formatted `display` string for anything that must stay exact
// (ADR-0003).

export type HBarItem = {
  label: string;
  /** Bar geometry (relative magnitude). */
  value: number;
  /** Pre-formatted value label; falls back to value.toLocaleString(). */
  display?: string;
  /** Secondary annotation rendered after the value (muted). */
  annotation?: string;
  /** Explicit bar color (e.g. a CATEGORICAL_PALETTE hue). Default: brand. */
  color?: string;
  /**
   * Render a short hatched tail on the bar end — the "lower bound"
   * marker (e.g. TVL with unpriced pools). Pair with an annotation
   * saying WHY it's a lower bound.
   */
  hatchTail?: boolean;
  /** Tooltip for the row (native title). */
  title?: string;
};

const HATCH_BG =
  'repeating-linear-gradient(135deg, currentColor 0, currentColor 2px, transparent 2px, transparent 5px)';

/**
 * HBarList — labeled horizontal bars on a shared linear scale (max of the
 * set). For magnitude comparisons across a small set of entities
 * (ops-by-type, auctions per pool, TVL per protocol). Not for
 * composition-of-a-whole — that's DonutChart.
 */
export function HBarList({
  items,
  formatValue = (n) => n.toLocaleString(),
  ariaLabel,
  max: maxProp,
  className,
}: {
  items: HBarItem[];
  formatValue?: (n: number) => string;
  ariaLabel: string;
  /**
   * Fixed scale maximum (e.g. 100 for percentages). Default: the
   * largest item value.
   */
  max?: number;
  className?: string;
}) {
  const finite = items.filter((it) => Number.isFinite(it.value) && it.value >= 0);
  if (finite.length === 0) return null;
  const max = maxProp ?? Math.max(...finite.map((it) => it.value), 0);
  if (max <= 0) return null;

  return (
    <ul role="img" aria-label={ariaLabel} className={cn('space-y-1.5', className)}>
      {finite.map((it) => (
        <li key={it.label} className="grid grid-cols-[minmax(7rem,14rem)_1fr] items-center gap-3 text-xs" title={it.title}>
          <span className="truncate text-ink-body" title={it.label}>
            {it.label}
          </span>
          <span className="flex min-w-0 items-center gap-2">
            <span className="relative h-2.5 min-w-[2px] shrink-0 rounded-xs" style={{ width: `${Math.max((it.value / max) * 100, 0.75) * 0.7}%` }}>
              <span
                aria-hidden
                className="absolute inset-0 rounded-xs"
                style={{ backgroundColor: it.color ?? 'var(--color-brand-500)' }}
              />
              {it.hatchTail && (
                <span
                  aria-hidden
                  className="absolute inset-y-0 -right-2 w-2 rounded-r-xs text-ink-faint opacity-70"
                  style={{ backgroundImage: HATCH_BG }}
                />
              )}
            </span>
            <span className="whitespace-nowrap font-mono tabular-nums text-ink">
              {it.display ?? formatValue(it.value)}
            </span>
            {it.annotation && (
              <span className="truncate whitespace-nowrap text-ink-muted">{it.annotation}</span>
            )}
          </span>
        </li>
      ))}
    </ul>
  );
}

export type PairedBarRow = {
  label: string;
  a: number;
  b: number;
  /** Pre-formatted labels for a / b (exact strings where needed). */
  aDisplay?: string;
  bDisplay?: string;
  title?: string;
};

/**
 * PairedBars — two series compared per entity (supplied vs borrowed,
 * inbound vs outbound), rendered as two thin bars per row on ONE shared
 * scale. A legend row names the two series (identity is never
 * color-alone: the legend + per-bar value labels carry it too).
 */
export function PairedBars({
  rows,
  aLabel,
  bLabel,
  aColor = 'var(--color-brand-500)',
  bColor = 'var(--color-up)',
  formatValue = (n) => n.toLocaleString(),
  ariaLabel,
  className,
}: {
  rows: PairedBarRow[];
  aLabel: string;
  bLabel: string;
  aColor?: string;
  bColor?: string;
  formatValue?: (n: number) => string;
  ariaLabel: string;
  className?: string;
}) {
  const finite = rows.filter((r) => Number.isFinite(r.a) && Number.isFinite(r.b));
  if (finite.length === 0) return null;
  const max = Math.max(...finite.flatMap((r) => [r.a, r.b]), 0);
  if (max <= 0) return null;

  return (
    <div className={cn('space-y-2', className)}>
      <ul className="flex flex-wrap gap-x-4 gap-y-1 text-xs text-ink-muted">
        <li className="flex items-center gap-1.5">
          <span aria-hidden className="inline-block h-2 w-2 rounded-full" style={{ backgroundColor: aColor }} />
          {aLabel}
        </li>
        <li className="flex items-center gap-1.5">
          <span aria-hidden className="inline-block h-2 w-2 rounded-full" style={{ backgroundColor: bColor }} />
          {bLabel}
        </li>
      </ul>
      <ul role="img" aria-label={ariaLabel} className="space-y-2.5">
        {finite.map((r) => (
          <li key={r.label} className="grid grid-cols-[minmax(7rem,14rem)_1fr] items-center gap-3 text-xs" title={r.title}>
            <span className="truncate text-ink-body" title={r.label}>
              {r.label}
            </span>
            <span className="min-w-0 space-y-0.5">
              {(
                [
                  { v: r.a, d: r.aDisplay, c: aColor, s: aLabel },
                  { v: r.b, d: r.bDisplay, c: bColor, s: bLabel },
                ] as const
              ).map((half) => (
                <span key={half.s} className="flex items-center gap-2">
                  <span
                    aria-hidden
                    className="h-1.5 min-w-[2px] shrink-0 rounded-xs"
                    style={{
                      width: `${Math.max((half.v / max) * 100, 0.75) * 0.7}%`,
                      backgroundColor: half.c,
                    }}
                  />
                  <span className="whitespace-nowrap font-mono text-[11px] tabular-nums text-ink-muted">
                    {half.d ?? formatValue(half.v)}
                  </span>
                </span>
              ))}
            </span>
          </li>
        ))}
      </ul>
    </div>
  );
}

export type DivergingBucket = {
  label: string;
  /** Positive-direction magnitude (e.g. received). Rendered up. */
  pos: number;
  /** Negative-direction magnitude (e.g. sent). Rendered down. Pass the MAGNITUDE (>= 0). */
  neg: number;
};

/**
 * DivergingColumns — small vertical diverging column chart around a zero
 * baseline (in/out per day). Both directions share one linear scale.
 * SVG so the baseline and columns stay crisp at any width.
 */
export function DivergingColumns({
  buckets,
  posLabel,
  negLabel,
  formatValue = (n) => n.toLocaleString(),
  height = 120,
  ariaLabel,
  className,
}: {
  buckets: DivergingBucket[];
  posLabel: string;
  negLabel: string;
  formatValue?: (n: number) => string;
  height?: number;
  ariaLabel: string;
  className?: string;
}) {
  const finite = buckets.filter((b) => Number.isFinite(b.pos) && Number.isFinite(b.neg));
  if (finite.length === 0) return null;
  const max = Math.max(...finite.flatMap((b) => [b.pos, b.neg]), 0);
  if (max <= 0) return null;

  const half = (height - 16) / 2; // 16px reserved for x labels
  const mid = half;
  const colW = 100 / finite.length;

  return (
    <div className={cn('space-y-1.5', className)}>
      <ul className="flex flex-wrap gap-x-4 gap-y-1 text-xs text-ink-muted">
        <li className="flex items-center gap-1.5">
          <span aria-hidden className="inline-block h-2 w-2 rounded-full bg-up" />
          {posLabel}
        </li>
        <li className="flex items-center gap-1.5">
          <span aria-hidden className="inline-block h-2 w-2 rounded-full bg-down" />
          {negLabel}
        </li>
      </ul>
      <svg
        role="img"
        aria-label={ariaLabel}
        viewBox={`0 0 100 ${height}`}
        preserveAspectRatio="none"
        className="w-full"
        style={{ height }}
      >
        {/* zero baseline */}
        <line x1="0" x2="100" y1={mid} y2={mid} stroke="var(--color-line-strong)" strokeWidth="0.5" vectorEffect="non-scaling-stroke" />
        {finite.map((b, i) => {
          const x = i * colW + colW * 0.18;
          const w = colW * 0.64;
          const posH = (b.pos / max) * (half - 2);
          const negH = (b.neg / max) * (half - 2);
          return (
            <g key={b.label}>
              {b.pos > 0 && (
                <rect x={x} y={mid - posH} width={w} height={posH} fill="var(--color-up)">
                  <title>{`${b.label} — ${posLabel}: ${formatValue(b.pos)}`}</title>
                </rect>
              )}
              {b.neg > 0 && (
                <rect x={x} y={mid} width={w} height={negH} fill="var(--color-down)">
                  <title>{`${b.label} — ${negLabel}: ${formatValue(b.neg)}`}</title>
                </rect>
              )}
            </g>
          );
        })}
      </svg>
      <div className="flex justify-between font-mono text-[10px] text-ink-faint">
        <span>{finite[0].label}</span>
        {finite.length > 1 && <span>{finite[finite.length - 1].label}</span>}
      </div>
    </div>
  );
}
