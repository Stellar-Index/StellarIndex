'use client';

import { useState } from 'react';

import { cn } from '@/lib/cn';

export type ScatterPoint = {
  id: string;
  /** Plot coordinates only; tooltip lines carry any exact figure. */
  x: number;
  y: number;
  /** Relative magnitude for dot AREA; null draws the minimum dot. */
  size?: number | null;
  color: string;
  /** Hollow ring instead of a filled dot (e.g. a rate a new position cannot get). */
  hollow?: boolean;
  /** Tooltip lines; the first is the heading. */
  lines: string[];
};

const R_MIN = 4;
const R_MAX = 18;

/** Dot radius in px: area proportional to size over the set's largest size. */
export function dotRadius(size: number | null | undefined, maxSize: number) {
  if (size == null || !(size > 0) || !(maxSize > 0)) return R_MIN;
  return Math.max(R_MIN, R_MAX * Math.sqrt(size / maxSize));
}

/**
 * Scatter — dots on a linear x/y plane, area by an optional size. HTML marks
 * on a percentage grid (like Bars), so dots stay round at any width and the
 * axis text keeps its size on a phone. Hover or focus shows a tooltip.
 */
export function Scatter({
  points,
  xMax = 100,
  yMax: yMaxProp,
  xLabel,
  yLabel,
  formatX = (n) => String(n),
  formatY = (n) => String(n),
  ariaLabel,
  height = 240,
  className,
}: {
  points: ScatterPoint[];
  xMax?: number;
  yMax?: number;
  xLabel: string;
  yLabel: string;
  formatX?: (n: number) => string;
  formatY?: (n: number) => string;
  ariaLabel: string;
  height?: number;
  className?: string;
}) {
  const [active, setActive] = useState<string | null>(null);
  const finite = points.filter(
    (p) => Number.isFinite(p.x) && Number.isFinite(p.y),
  );
  if (finite.length === 0) return null;
  const yMax = yMaxProp ?? Math.max(...finite.map((p) => p.y), 0) * 1.08;
  const maxSize = Math.max(0, ...finite.map((p) => p.size ?? 0));
  const ySpan = yMax > 0 ? yMax : 1;
  const xPct = (x: number) => (Math.min(Math.max(x, 0), xMax) / xMax) * 100;
  const yPct = (y: number) => (Math.min(Math.max(y, 0), ySpan) / ySpan) * 100;
  // Largest first so small dots stay on top and hoverable.
  const drawn = [...finite].sort((a, b) => (b.size ?? 0) - (a.size ?? 0));
  const shown = drawn.find((p) => p.id === active);
  const yTicks = [0, ySpan / 2, ySpan];

  return (
    <figure className={cn('space-y-1', className)}>
      <div className="text-ink-muted text-xs">{yLabel}</div>
      <div className="grid grid-cols-[auto_minmax(0,1fr)] gap-2">
        <div
          className="text-ink-faint relative w-10 font-mono text-[10px]"
          style={{ height }}
          aria-hidden
        >
          {yTicks.map((t) => (
            <span
              key={t}
              className="absolute right-0 translate-y-1/2"
              style={{ bottom: `${yPct(t)}%` }}
            >
              {formatY(t)}
            </span>
          ))}
        </div>
        <div
          role="group"
          aria-label={ariaLabel}
          className="border-line-strong relative border-b border-l"
          style={{ height }}
          onMouseLeave={() => setActive(null)}
        >
          {yTicks.slice(1).map((t) => (
            <span
              key={t}
              aria-hidden
              className="border-line absolute inset-x-0 border-t"
              style={{ bottom: `${yPct(t)}%` }}
            />
          ))}
          {drawn.map((p) => {
            const r = dotRadius(p.size, maxSize);
            const hit = Math.max(r * 2, 20);
            return (
              <span
                key={p.id}
                role="img"
                tabIndex={0}
                aria-label={p.lines.join(', ')}
                data-testid="scatter-dot"
                className="absolute flex -translate-x-1/2 translate-y-1/2 items-center justify-center rounded-full outline-none focus-visible:ring-2"
                style={{
                  left: `${xPct(p.x)}%`,
                  bottom: `${yPct(p.y)}%`,
                  width: hit,
                  height: hit,
                }}
                onMouseEnter={() => setActive(p.id)}
                onFocus={() => setActive(p.id)}
                onBlur={() => setActive(null)}
              >
                <span
                  aria-hidden
                  className="block rounded-full"
                  style={{
                    width: r * 2,
                    height: r * 2,
                    backgroundColor: p.hollow ? 'transparent' : p.color,
                    opacity: p.hollow ? 1 : 0.85,
                    border: p.hollow
                      ? `2px solid ${p.color}`
                      : '2px solid var(--color-surface)',
                  }}
                />
              </span>
            );
          })}
          {shown && (
            <div
              role="tooltip"
              className="bg-surface-muted border-line-strong pointer-events-none absolute z-10 w-max max-w-[14rem] -translate-x-1/2 rounded-sm border px-2 py-1 text-xs"
              style={{
                left: `${Math.min(Math.max(xPct(shown.x), 15), 85)}%`,
                bottom: `calc(${Math.min(yPct(shown.y), 70)}% + 1.5rem)`,
              }}
            >
              <div className="text-ink font-medium">{shown.lines[0]}</div>
              {shown.lines.slice(1).map((l) => (
                <div key={l} className="text-ink-muted font-mono">
                  {l}
                </div>
              ))}
            </div>
          )}
        </div>
      </div>
      <div className="text-ink-faint ml-12 flex justify-between font-mono text-[10px]">
        <span>{formatX(0)}</span>
        <span>{formatX(xMax / 2)}</span>
        <span>{formatX(xMax)}</span>
      </div>
      <figcaption className="text-ink-muted ml-12 text-center text-xs">
        {xLabel}
      </figcaption>
    </figure>
  );
}
