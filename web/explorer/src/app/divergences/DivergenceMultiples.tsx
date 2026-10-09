'use client';

import { useQueries } from '@tanstack/react-query';

import { apiGet } from '@/api/client';
import { shortAssetText } from '@/lib/asset-label';
import type { paths } from '@/api/types';

type SeriesResp = NonNullable<
  paths['/divergence/series']['get']['responses'][200]['content']['application/json']['data']
>;

export type MultiplePair = { asset: string; quote: string };

const W = 160;
const H = 48;

/** Per bucket, the signed Δ% of the reference furthest from ours; null where none compared. */
export function widestDelta(
  points: SeriesResp['points'] | undefined,
): (number | null)[] {
  return (points ?? []).map((p) => {
    let best: number | null = null;
    for (const r of p.references ?? []) {
      const v = Number(r.delta_pct);
      if (Number.isFinite(v) && (best == null || Math.abs(v) > Math.abs(best)))
        best = v;
    }
    return best;
  });
}

/** SVG path for the series around a zero line; gaps break the line. */
export function multiplePath(
  values: readonly (number | null)[],
  bound: number,
) {
  if (values.length < 2 || !(bound > 0)) return '';
  const step = W / (values.length - 1);
  let d = '';
  let pen = false;
  values.forEach((v, i) => {
    if (v == null) {
      pen = false;
      return;
    }
    const y = H / 2 - (Math.max(-bound, Math.min(bound, v)) / bound) * (H / 2);
    d += `${pen ? 'L' : 'M'}${(i * step).toFixed(1)},${y.toFixed(1)}`;
    pen = true;
  });
  return d;
}

export function DivergenceMultiples({
  pairs,
  onSelect,
}: {
  pairs: readonly MultiplePair[];
  onSelect: (p: MultiplePair) => void;
}) {
  const qs = useQueries({
    queries: pairs.map((p) => ({
      queryKey: ['/v1/divergence/series', `${p.asset}~${p.quote}`, 7],
      queryFn: async () =>
        (
          await apiGet<{ data: SeriesResp }>('/v1/divergence/series', {
            pair: `${p.asset}~${p.quote}`,
            days: 7,
          })
        ).data,
      staleTime: 60_000,
    })),
  });
  const series = qs.map((q) => ({
    values: widestDelta(q.data?.points),
    threshold: q.data?.threshold_pct,
  }));
  // One shared scale so a flat pair reads flat beside a volatile one.
  const bound = Math.max(
    0,
    ...series.flatMap((s) => [
      ...s.values.map((v) => (v == null ? 0 : Math.abs(v))),
      s.threshold ?? 0,
    ]),
  );
  if (bound === 0) return null;

  return (
    <div className="grid grid-cols-2 gap-3 sm:grid-cols-3">
      {pairs.map((p, i) => {
        const s = series[i];
        const label = `${shortAssetText(p.asset)}/${shortAssetText(p.quote)}`;
        const ty =
          s.threshold != null && s.threshold > 0
            ? (s.threshold / bound) * (H / 2)
            : null;
        return (
          <button
            key={`${p.asset}~${p.quote}`}
            type="button"
            onClick={() => onSelect(p)}
            className="border-line hover:border-brand-500 rounded-lg border p-2 text-left"
            aria-label={`Plot ${label} divergence history`}
          >
            <div className="text-ink-body truncate font-mono text-xs">
              {label}
            </div>
            <svg
              viewBox={`0 0 ${W} ${H}`}
              className="mt-1 h-12 w-full"
              aria-hidden
            >
              {ty != null && (
                <rect
                  x={0}
                  y={H / 2 - ty}
                  width={W}
                  height={2 * ty}
                  className="fill-surface-muted"
                />
              )}
              <line
                x1={0}
                x2={W}
                y1={H / 2}
                y2={H / 2}
                className="stroke-line"
              />
              <path
                d={multiplePath(s.values, bound)}
                fill="none"
                className="stroke-brand-500"
                strokeWidth={1.5}
              />
            </svg>
          </button>
        );
      })}
    </div>
  );
}
