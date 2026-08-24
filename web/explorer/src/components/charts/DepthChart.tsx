'use client';

import { useMemo, useRef, useState } from 'react';

import { cn } from '@/lib/cn';
import { formatSubunitPrice } from '@/lib/format';

/**
 * One aggregated order-book level, as served by GET /v1/sdex/orderbook
 * (`SDEXOrderBookLevel`). Prices and amounts are exact decimal strings;
 * this chart converts to Number for LAYOUT ONLY — the tables alongside
 * keep the exact strings (CLAUDE.md invariant #1 is about arithmetic and
 * storage, not pixel positioning).
 */
export type DepthLevel = {
  price: string;
  cum_base_amount: string;
  cum_quote_amount: string;
};

export type BookStats = {
  bestBid: number | null;
  bestAsk: number | null;
  /** bestAsk − bestBid; negative when the served book is crossed. */
  spread: number | null;
  /** (bestBid + bestAsk) / 2. */
  mid: number | null;
  /** spread / mid in basis points. */
  spreadBps: number | null;
  /** True when bestBid ≥ bestAsk — the snapshot's sides overlap. */
  crossed: boolean;
};

/**
 * computeBookStats — spread / mid from the top of each served side.
 * `bids` arrive best (highest) first, `asks` best (lowest) first — the
 * /v1/sdex/orderbook contract. Missing sides yield nulls, never zeros.
 */
export function computeBookStats(
  bids: DepthLevel[],
  asks: DepthLevel[],
): BookStats {
  const bestBid = bids.length > 0 ? toFinite(bids[0].price) : null;
  const bestAsk = asks.length > 0 ? toFinite(asks[0].price) : null;
  if (bestBid == null || bestAsk == null) {
    return {
      bestBid,
      bestAsk,
      spread: null,
      mid: null,
      spreadBps: null,
      crossed: false,
    };
  }
  const spread = bestAsk - bestBid;
  const mid = (bestBid + bestAsk) / 2;
  const spreadBps = mid > 0 ? (spread / mid) * 10_000 : null;
  return { bestBid, bestAsk, spread, mid, spreadBps, crossed: spread <= 0 };
}

function toFinite(s: string): number | null {
  const n = Number(s);
  return Number.isFinite(n) ? n : null;
}

type SidePoint = { price: number; cum: number; cumQuote: number };

function toSide(levels: DepthLevel[]): SidePoint[] {
  const out: SidePoint[] = [];
  for (const l of levels) {
    const price = toFinite(l.price);
    const cum = toFinite(l.cum_base_amount);
    const cumQuote = toFinite(l.cum_quote_amount);
    if (price == null || cum == null) continue;
    out.push({ price, cum, cumQuote: cumQuote ?? 0 });
  }
  return out;
}

/**
 * DepthChart — mirrored cumulative order-book depth ("how much size
 * before I eat N bps of slippage?"). Bids fill from the best bid
 * leftward (up tone), asks from the best ask rightward (down tone),
 * both as step areas of cumulative BASE amount over price. Pure SVG on
 * the design tokens — no chart library. Sides are identified by
 * position AND direct labels (never colour alone); a crossed snapshot
 * (best bid ≥ best ask) renders both sides honestly and lets the
 * companion stat strip carry the warning.
 *
 * Renders nothing when neither side has a plottable level — callers
 * show their own empty state.
 */
export function DepthChart({
  bids,
  asks,
  baseLabel,
  quoteLabel,
  height = 224,
  className,
}: {
  bids: DepthLevel[];
  asks: DepthLevel[];
  baseLabel: string;
  quoteLabel: string;
  height?: number;
  className?: string;
}) {
  const containerRef = useRef<HTMLDivElement>(null);
  const [hover, setHover] = useState<{
    frac: number;
    price: number;
    side: 'bid' | 'ask' | null;
    cum: number | null;
    cumQuote: number | null;
  } | null>(null);

  const model = useMemo(() => {
    const bidPts = toSide(bids); // best (highest price) first
    const askPts = toSide(asks); // best (lowest price) first
    if (bidPts.length === 0 && askPts.length === 0) return null;
    const prices = [...bidPts, ...askPts].map((p) => p.price);
    let min = Math.min(...prices);
    let max = Math.max(...prices);
    if (min === max) {
      // Degenerate single-price book — pad so the area has width.
      min *= 0.999;
      max *= 1.001;
    }
    const maxCum = Math.max(
      bidPts.length > 0 ? bidPts[bidPts.length - 1].cum : 0,
      askPts.length > 0 ? askPts[askPts.length - 1].cum : 0,
    );
    return { bidPts, askPts, min, max, span: max - min, maxCum: maxCum || 1 };
  }, [bids, asks]);

  if (!model) return null;
  const { bidPts, askPts, min, span, maxCum } = model;

  // Geometry in a 0–100 × 0–100 viewBox stretched to fill the container
  // (preserveAspectRatio="none"); axis text lives in HTML so nothing
  // distorts.
  const X = (price: number) => ((price - min) / span) * 100;
  const Y = (cum: number) => 100 - (cum / maxCum) * 96; // 4% headroom

  // Bids: depth at price p = cum of all bids priced ≥ p. Steps run from
  // the worst (lowest) served bid up to the best, then drop to zero.
  const bidPath = stepPath(
    [...bidPts].reverse().map((p) => ({ x: X(p.price), y: Y(p.cum) })),
    'bid',
  );
  // Asks: depth at price p = cum of all asks priced ≤ p.
  const askPath = stepPath(
    askPts.map((p) => ({ x: X(p.price), y: Y(p.cum) })),
    'ask',
  );

  function onMove(e: React.MouseEvent<HTMLDivElement>) {
    const el = containerRef.current;
    if (!el) return;
    const rect = el.getBoundingClientRect();
    const frac = Math.min(1, Math.max(0, (e.clientX - rect.left) / rect.width));
    const price = min + frac * span;
    // Depth under the cursor: bids fill for prices ≤ best bid, asks for
    // prices ≥ best ask. Both may match on a crossed snapshot — prefer
    // the nearer best.
    const bid = depthAt(bidPts, price, 'bid');
    const ask = depthAt(askPts, price, 'ask');
    let side: 'bid' | 'ask' | null = null;
    if (bid && ask) {
      side =
        Math.abs(price - bidPts[0].price) <= Math.abs(price - askPts[0].price)
          ? 'bid'
          : 'ask';
    } else if (bid) {
      side = 'bid';
    } else if (ask) {
      side = 'ask';
    }
    const at = side === 'bid' ? bid : side === 'ask' ? ask : null;
    setHover({
      frac,
      price,
      side,
      cum: at?.cum ?? null,
      cumQuote: at?.cumQuote ?? null,
    });
  }

  return (
    <div className={className}>
      <div className="mb-1 flex items-baseline justify-between font-mono text-[10px] tracking-wider uppercase">
        <span className="text-up">Bids</span>
        <span className="text-ink-faint">cumulative {baseLabel} depth</span>
        <span className="text-down">Asks</span>
      </div>
      <div
        ref={containerRef}
        className="border-line-subtle bg-surface-subtle/40 relative w-full overflow-hidden rounded-sm border"
        style={{ height }}
        onMouseMove={onMove}
        onMouseLeave={() => setHover(null)}
        role="img"
        aria-label={`Cumulative order-book depth chart: ${bidPts.length} bid levels and ${askPts.length} ask levels of ${baseLabel} priced in ${quoteLabel}`}
      >
        <svg
          className="absolute inset-0 h-full w-full"
          viewBox="0 0 100 100"
          preserveAspectRatio="none"
          aria-hidden
        >
          {bidPath && (
            <path
              d={bidPath}
              fill="var(--color-up)"
              fillOpacity={0.16}
              stroke="var(--color-up)"
              strokeWidth={1.5}
              vectorEffect="non-scaling-stroke"
              strokeLinejoin="round"
            />
          )}
          {askPath && (
            <path
              d={askPath}
              fill="var(--color-down)"
              fillOpacity={0.16}
              stroke="var(--color-down)"
              strokeWidth={1.5}
              vectorEffect="non-scaling-stroke"
              strokeLinejoin="round"
            />
          )}
        </svg>
        {hover && (
          <>
            <div
              aria-hidden
              className="bg-line-strong pointer-events-none absolute inset-y-0 w-px"
              style={{ left: `${hover.frac * 100}%` }}
            />
            <div
              className="border-line bg-surface/95 text-ink-body shadow-card pointer-events-none absolute top-2 z-10 rounded-sm border px-2 py-1 font-mono text-[11px]"
              style={
                hover.frac > 0.55
                  ? { right: `${(1 - hover.frac) * 100}%`, marginRight: 8 }
                  : { left: `${hover.frac * 100}%`, marginLeft: 8 }
              }
            >
              <div className="text-ink-muted">
                {formatDepthPrice(hover.price)} {quoteLabel}
              </div>
              {hover.side && hover.cum != null ? (
                <div className={hover.side === 'bid' ? 'text-up' : 'text-down'}>
                  {hover.side === 'bid' ? 'bid' : 'ask'} depth{' '}
                  {formatDepthAmount(hover.cum)} {baseLabel}
                  {hover.cumQuote != null && hover.cumQuote > 0 && (
                    <span className="text-ink-muted">
                      {' '}
                      · {formatDepthAmount(hover.cumQuote)} {quoteLabel}
                    </span>
                  )}
                </div>
              ) : (
                <div className="text-ink-faint">no depth at this price</div>
              )}
            </div>
          </>
        )}
      </div>
      <div className="text-ink-muted mt-1 flex justify-between font-mono text-[10px] tabular-nums">
        <span>{formatDepthPrice(min)}</span>
        <span className="text-ink-faint">
          price ({quoteLabel} per {baseLabel})
        </span>
        <span>{formatDepthPrice(min + span)}</span>
      </div>
    </div>
  );
}

/**
 * OrderBookStatStrip — best bid / best ask / mid / spread over the top
 * of the served book. A crossed snapshot (best bid ≥ best ask) renders
 * an explicit warning instead of passing off a negative spread as a
 * market fact; missing sides render "—".
 */
export function OrderBookStatStrip({
  stats,
  quoteLabel,
}: {
  stats: BookStats;
  quoteLabel: string;
}) {
  return (
    <div>
      <dl className="grid grid-cols-2 gap-3 text-sm sm:grid-cols-4">
        <StatCell
          label="Best bid"
          value={stats.bestBid != null ? formatDepthPrice(stats.bestBid) : '—'}
          tone="up"
        />
        <StatCell
          label="Best ask"
          value={stats.bestAsk != null ? formatDepthPrice(stats.bestAsk) : '—'}
          tone="down"
        />
        <StatCell
          label={`Mid (${quoteLabel})`}
          value={
            !stats.crossed && stats.mid != null
              ? formatDepthPrice(stats.mid)
              : '—'
          }
        />
        <StatCell
          label="Spread"
          value={
            !stats.crossed && stats.spread != null && stats.spreadBps != null
              ? `${formatDepthPrice(stats.spread)} · ${stats.spreadBps.toFixed(1)} bps`
              : '—'
          }
        />
      </dl>
      {stats.crossed && (
        <p className="bg-warn-50 text-warn-700 mt-2 rounded-sm px-2 py-1 text-xs">
          The served snapshot is crossed (best bid ≥ best ask), so mid and
          spread aren&rsquo;t meaningful market facts for it — both sides are
          drawn as served.
        </p>
      )}
    </div>
  );
}

function StatCell({
  label,
  value,
  tone,
}: {
  label: string;
  value: string;
  tone?: 'up' | 'down';
}) {
  return (
    <div>
      <dt className="text-ink-muted text-[11px] tracking-wider uppercase">
        {label}
      </dt>
      <dd
        className={cn(
          'mt-0.5 font-mono text-sm tabular-nums',
          tone === 'up' && 'text-up',
          tone === 'down' && 'text-down',
        )}
      >
        {value}
      </dd>
    </div>
  );
}

// stepPath builds the step-area path for one side. `pts` are ordered by
// ascending x; y is the depth at-and-beyond that point in the side's
// fill direction. Bids fill leftward from the best bid (last point);
// asks rightward from the best ask (first point).
function stepPath(
  pts: { x: number; y: number }[],
  side: 'bid' | 'ask',
): string | null {
  if (pts.length === 0) return null;
  const parts: string[] = [];
  parts.push(`M ${f(pts[0].x)} 100`, `L ${f(pts[0].x)} ${f(pts[0].y)}`);
  if (side === 'bid') {
    // Bids ascend in price left→right while cumulative depth SHRINKS:
    // between two bid prices the depth is the shallower (next-better)
    // cumulative, so the vertical drop happens at the LEFT point.
    for (let i = 1; i < pts.length; i++) {
      parts.push(
        `L ${f(pts[i - 1].x)} ${f(pts[i].y)}`,
        `L ${f(pts[i].x)} ${f(pts[i].y)}`,
      );
    }
  } else {
    // Asks ascend in price AND depth: between two ask prices the depth
    // is the previous cumulative, so the rise happens at the RIGHT point.
    for (let i = 1; i < pts.length; i++) {
      parts.push(
        `L ${f(pts[i].x)} ${f(pts[i - 1].y)}`,
        `L ${f(pts[i].x)} ${f(pts[i].y)}`,
      );
    }
  }
  parts.push(`L ${f(pts[pts.length - 1].x)} 100`, 'Z');
  return parts.join(' ');
}

function f(n: number): string {
  return n.toFixed(2);
}

// depthAt — the cumulative depth covering `price` on one side, or null
// when the price is outside the side's served range.
function depthAt(
  pts: SidePoint[],
  price: number,
  side: 'bid' | 'ask',
): { cum: number; cumQuote: number } | null {
  if (pts.length === 0) return null;
  if (side === 'bid') {
    // pts best (highest) first; depth at p = cum of first level with price ≥ p.
    if (price > pts[0].price || price < pts[pts.length - 1].price) return null;
    let acc: SidePoint | null = null;
    for (const p of pts) {
      if (p.price >= price) acc = p;
      else break;
    }
    return acc ? { cum: acc.cum, cumQuote: acc.cumQuote } : null;
  }
  // asks: best (lowest) first; depth at p = cum of last level with price ≤ p.
  if (price < pts[0].price || price > pts[pts.length - 1].price) return null;
  let acc: SidePoint | null = null;
  for (const p of pts) {
    if (p.price <= price) acc = p;
    else break;
  }
  return acc ? { cum: acc.cum, cumQuote: acc.cumQuote } : null;
}

/** Adaptive price formatter shared by the chart, axis, and stat strip.
 * FEC audit F-A4-03: the sub-1e-4 tail previously used toExponential(3),
 * violating the recorded 2026-08-06 operator decision (no scientific
 * notation anywhere a price renders); it now shares the canonical
 * plain-decimal subunit path. */
export function formatDepthPrice(n: number): string {
  if (!Number.isFinite(n)) return '—';
  const abs = Math.abs(n);
  if (abs >= 1000) return n.toFixed(2);
  if (abs >= 1) return n.toFixed(4);
  if (abs >= 0.0001) return n.toFixed(7).replace(/0+$/, '').replace(/\.$/, '');
  if (abs === 0) return '0';
  return formatSubunitPrice(n);
}

function formatDepthAmount(n: number): string {
  if (!Number.isFinite(n)) return '—';
  if (n >= 1_000_000) return `${(n / 1_000_000).toFixed(2)}M`;
  if (n >= 1_000) return `${(n / 1_000).toFixed(1)}k`;
  return n.toLocaleString('en-US', { maximumFractionDigits: 2 });
}
