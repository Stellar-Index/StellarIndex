'use client';

import { CATEGORICAL_PALETTE } from '@/components/charts/DonutChart';
import { baseUnitsDecimal, formatPairPrice } from '@/lib/format';

export type ScatterTrade = {
  ts: string;
  source: string;
  price?: string | null;
  base_amount?: string | null;
  base_decimals?: number | null;
};

export type ScatterDot = {
  x: number;
  y: number;
  r: number;
  source: string;
  title: string;
};

const W = 600;
const H = 160;
const PAD = 6;
const R_MIN = 2;
const R_MAX = 9;

/** Plots trades as time × price with area ∝ base amount; numbers here are geometry only. */
export function scatterDots(trades: readonly ScatterTrade[]): {
  dots: ScatterDot[];
  sources: string[];
  range: { minT: number; maxT: number; minP: number; maxP: number };
} | null {
  const pts = trades
    .map((t) => {
      const amount = baseUnitsDecimal(t.base_amount, t.base_decimals ?? 7);
      return {
        t,
        ms: new Date(t.ts).getTime(),
        price: Number(t.price),
        size: amount == null ? 0 : Math.abs(Number(amount)),
        amount,
      };
    })
    .filter(
      (p) => Number.isFinite(p.ms) && Number.isFinite(p.price) && p.price > 0,
    );
  if (pts.length < 2) return null;
  const minT = Math.min(...pts.map((p) => p.ms));
  const maxT = Math.max(...pts.map((p) => p.ms));
  const minP = Math.min(...pts.map((p) => p.price));
  const maxP = Math.max(...pts.map((p) => p.price));
  const maxSize = Math.max(...pts.map((p) => p.size));
  const sources = [...new Set(pts.map((p) => p.t.source))].sort();
  const dots = pts.map((p) => ({
    x:
      PAD +
      (maxT > minT
        ? ((p.ms - minT) / (maxT - minT)) * (W - 2 * PAD)
        : (W - 2 * PAD) / 2),
    y:
      PAD +
      (maxP > minP
        ? (1 - (p.price - minP) / (maxP - minP)) * (H - 2 * PAD)
        : (H - 2 * PAD) / 2),
    r:
      maxSize > 0
        ? R_MIN + Math.sqrt(p.size / maxSize) * (R_MAX - R_MIN)
        : R_MIN,
    source: p.t.source,
    title: `${p.t.ts} · ${p.t.source} · ${p.t.price}${p.amount ? ` · ${p.amount}` : ''}`,
  }));
  return { dots, sources, range: { minT, maxT, minP, maxP } };
}

export function TradeScatter({ trades }: { trades: readonly ScatterTrade[] }) {
  const s = scatterDots(trades);
  if (!s) return null;
  const hue = (src: string) =>
    CATEGORICAL_PALETTE[
      s.sources.indexOf(src) % (CATEGORICAL_PALETTE.length - 1)
    ];
  return (
    <figure className="mb-4 space-y-2">
      <svg
        height={H}
        className="border-line-subtle bg-surface-subtle/40 h-40 w-full rounded-sm border"
        role="img"
        aria-label={`Recent trades plotted by time and price, sized by base amount, across ${s.sources.length} source${s.sources.length === 1 ? '' : 's'}`}
      >
        {s.dots.map((d, i) => (
          <circle
            key={i}
            cx={`${(d.x / W) * 100}%`}
            cy={d.y}
            r={d.r}
            fill={hue(d.source)}
            fillOpacity={0.6}
          >
            <title>{d.title}</title>
          </circle>
        ))}
      </svg>
      <div className="text-ink-muted flex justify-between gap-3 font-mono text-xs tabular-nums">
        <span>{formatClock(s.range.minT, s.range)}</span>
        <span>
          {formatPairPrice(s.range.minP)} to {formatPairPrice(s.range.maxP)}
        </span>
        <span>{formatClock(s.range.maxT, s.range)}</span>
      </div>
      <figcaption className="text-ink-muted flex flex-wrap gap-3 text-xs">
        {s.sources.map((src) => (
          <span key={src} className="inline-flex items-center gap-1.5">
            <span
              className="inline-block h-2 w-2 rounded-full"
              style={{ background: hue(src) }}
            />
            {src}
          </span>
        ))}
        <span>Dot area scales with base amount</span>
      </figcaption>
    </figure>
  );
}

// Clock only while the sample sits inside one day; the date joins once it
// spans more.
function formatClock(ms: number, range: { minT: number; maxT: number }) {
  const iso = new Date(ms).toISOString();
  return range.maxT - range.minT > 86_400_000
    ? `${iso.slice(0, 16).replace('T', ' ')} UTC`
    : `${iso.slice(11, 19)} UTC`;
}
