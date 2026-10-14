'use client';

import { useState } from 'react';

import { Segmented } from '@/components/ui';
import { PANEL_LABEL_CLASS } from '@/components/ui/Page';

interface Point {
  t: string;
  p?: string | null;
}

/**
 * PriceSparklines — toggle between 24h and 7d USD-price sparklines.
 * Falls back to whichever series is populated when one window is
 * empty. Hides itself entirely when both are too sparse to plot.
 */
export function PriceSparklines({
  points24h,
  points7d,
}: {
  points24h: Point[];
  points7d: Point[];
}) {
  const has24 = points24h.filter((p) => p.p != null).length >= 2;
  const has7 = points7d.filter((p) => p.p != null).length >= 2;
  const [pref, setPref] = useState<'24h' | '7d'>('24h');
  if (!has24 && !has7) return null;
  const active = !has24 ? '7d' : !has7 ? '24h' : pref;
  const points = active === '24h' ? points24h : points7d;

  return (
    <div className="border-line border-t pt-3">
      <div className="mb-2 flex items-center justify-between gap-2">
        <span className={PANEL_LABEL_CLASS}>Price</span>
        {has24 && has7 ? (
          <Segmented
            ariaLabel="Sparkline window"
            options={[
              { label: '24h', value: '24h' },
              { label: '7d', value: '7d' },
            ]}
            value={active}
            onChange={(v) => setPref(v as '24h' | '7d')}
          />
        ) : (
          <span className={PANEL_LABEL_CLASS}>{active}</span>
        )}
      </div>
      <Sparkline points={points} ariaLabel={`${active} price sparkline`} />
    </div>
  );
}

function Sparkline({
  points,
  ariaLabel,
}: {
  points: Point[];
  ariaLabel: string;
}) {
  const values = points.map((pt) => {
    const n = pt.p ? Number(pt.p) : null;
    return n != null && Number.isFinite(n) ? n : null;
  });
  const finite = values.filter((v): v is number => v != null);
  if (finite.length < 2) return null;
  const min = Math.min(...finite);
  const max = Math.max(...finite);
  const range = max - min || 1;
  const W = 600;
  const H = 60;
  const segments: string[] = [];
  // Track each contiguous run so the area fill can close back to
  // the baseline at gap boundaries — null buckets break the shape.
  type Run = { start: number; end: number };
  const runs: Run[] = [];
  let pen = false;
  let runStart = -1;
  let lastIdx = -1;
  values.forEach((v, i) => {
    const x = (i / (values.length - 1)) * W;
    if (v == null) {
      if (pen && runStart >= 0) runs.push({ start: runStart, end: lastIdx });
      pen = false;
      runStart = -1;
      return;
    }
    const y = H - ((v - min) / range) * H;
    segments.push(`${pen ? 'L' : 'M'} ${x.toFixed(1)} ${y.toFixed(1)}`);
    if (!pen) runStart = i;
    pen = true;
    lastIdx = i;
  });
  if (pen && runStart >= 0) runs.push({ start: runStart, end: lastIdx });
  const last = finite[finite.length - 1];
  const first = finite[0];
  const positive = last >= first;
  const tone = positive ? 'stroke-up' : 'stroke-down';
  const fill = positive ? 'fill-up/15' : 'fill-down/15';
  // Area path: emit one closed sub-region per contiguous run.
  const areaSegs: string[] = [];
  for (const run of runs) {
    let started = false;
    for (let i = run.start; i <= run.end; i++) {
      const v = values[i];
      if (v == null) continue;
      const x = (i / (values.length - 1)) * W;
      const y = H - ((v - min) / range) * H;
      areaSegs.push(`${started ? 'L' : 'M'} ${x.toFixed(1)} ${y.toFixed(1)}`);
      started = true;
    }
    const xStart = (run.start / (values.length - 1)) * W;
    const xEnd = (run.end / (values.length - 1)) * W;
    areaSegs.push(`L ${xEnd.toFixed(1)} ${H} L ${xStart.toFixed(1)} ${H} Z`);
  }
  return (
    <svg
      viewBox={`0 0 ${W} ${H}`}
      preserveAspectRatio="none"
      className="h-12 w-full"
      role="img"
      aria-label={ariaLabel}
    >
      <path d={areaSegs.join(' ')} stroke="none" className={fill} />
      <path
        d={segments.join(' ')}
        fill="none"
        strokeWidth="1.5"
        className={tone}
      />
    </svg>
  );
}
