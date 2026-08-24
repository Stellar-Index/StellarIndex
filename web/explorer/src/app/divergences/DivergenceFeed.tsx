'use client';

import { useState } from 'react';
import dynamic from 'next/dynamic';
import { useQuery } from '@tanstack/react-query';

import { Panel } from '@/components/reveal';
import { AssetText } from '@/components/AssetLink';
import { shortAssetText } from '@/lib/asset-label';
import { apiGet, asExample } from '@/api/client';
import { HBarList } from '@/components/charts/Bars';
import type { paths } from '@/api/types';

const LineChart = dynamic(
  () => import('@/components/charts/LineChart').then((m) => m.LineChart),
  { ssr: false, loading: () => <div className="h-[260px]" /> },
);

// GET /v1/divergence + /v1/divergence/series response bodies, derived
// from the generated OpenAPI contract (src/api/types.ts,
// `make web-generate-api`).
type DivergenceResp = NonNullable<
  paths['/divergence']['get']['responses'][200]['content']['application/json']['data']
>;
type DivergenceSeriesResp = NonNullable<
  paths['/divergence/series']['get']['responses'][200]['content']['application/json']['data']
>;

const SERIES_WINDOWS = [1, 7, 30] as const;

function fmtTs(iso: string): string {
  const d = new Date(iso);
  return Number.isNaN(d.getTime())
    ? iso
    : d.toISOString().replace('T', ' ').slice(0, 19) + 'Z';
}

function fmtDelta(s: string): string {
  const n = Number(s);
  if (!Number.isFinite(n)) return s;
  return `${n > 0 ? '+' : ''}${n.toFixed(2)}%`;
}

type Selection = { asset: string; quote: string; reference: string };

export function DivergenceFeed() {
  const [selected, setSelected] = useState<Selection | null>(null);
  const [days, setDays] = useState<(typeof SERIES_WINDOWS)[number]>(7);

  const q = useQuery<DivergenceResp>({
    queryKey: ['/v1/divergence'],
    queryFn: async () => {
      const env = await apiGet<{ data: DivergenceResp }>('/v1/divergence', {
        limit: 100,
        window_days: 7,
      });
      return env.data;
    },
    staleTime: 30_000,
    refetchInterval: 30_000,
  });

  const rows = q.data?.observations ?? [];
  // Default selection: the board's widest gap (the first row — the
  // API orders by |Δ%| desc).
  const sel: Selection | null =
    selected ??
    (rows[0]
      ? {
          asset: rows[0].asset_id ?? '',
          quote: rows[0].quote_id ?? '',
          reference: rows[0].reference ?? '',
        }
      : null);

  return (
    <>
      <DivergenceSeriesPanel sel={sel} days={days} onDays={setDays} />

      <BoardBars rows={rows} />

      <Panel
        title="Divergence board"
        hint="Latest comparison per (pair, reference) over the trailing 7 days — our VWAP vs each external reference, widest gap first. Click a row to plot its history above."
        source={asExample('/v1/divergence', { limit: 100, window_days: 7 })}
        bodyClassName="space-y-3"
      >
        {q.isLoading && <p className="text-ink-muted text-sm">Loading…</p>}
        {q.isError && (
          <p className="text-ink-muted text-sm">
            The divergence board is unavailable right now.
          </p>
        )}
        {q.data && rows.length === 0 && (
          <p className="text-ink-muted text-sm">
            No cross-reference comparisons recorded in the last 7 days (the
            divergence worker writes one row per configured (pair, reference)
            per tick).
          </p>
        )}
        {rows.length > 0 && (
          <table className="w-full text-sm">
            <thead>
              <tr className="border-line text-ink-muted border-b text-left text-[11px] tracking-wider uppercase">
                <th className="py-1.5 pr-4 font-normal">Pair</th>
                <th className="py-1.5 pr-4 font-normal">Reference</th>
                <th className="py-1.5 pr-4 text-right font-normal">
                  Our price
                </th>
                <th className="py-1.5 pr-4 text-right font-normal">
                  Reference
                </th>
                <th className="py-1.5 pr-4 text-right font-normal">Δ%</th>
                <th className="py-1.5 pr-4 font-normal">Observed</th>
                <th className="py-1.5 font-normal">State</th>
              </tr>
            </thead>
            <tbody>
              {rows.map((d) => {
                const firing = d.status === 'firing';
                const isSel =
                  sel != null &&
                  d.asset_id === sel.asset &&
                  d.quote_id === sel.quote &&
                  d.reference === sel.reference;
                return (
                  <tr
                    key={`${d.asset_id}:${d.quote_id}:${d.reference}`}
                    onClick={() =>
                      setSelected({
                        asset: d.asset_id ?? '',
                        quote: d.quote_id ?? '',
                        reference: d.reference ?? '',
                      })
                    }
                    className={`border-line/60 hover:bg-surface-muted cursor-pointer border-b last:border-0 ${
                      isSel ? 'bg-surface-muted' : ''
                    }`}
                  >
                    <td className="py-1.5 pr-4 font-mono">
                      <AssetText canonical={d.asset_id} />
                      <span className="text-ink-faint">/</span>
                      <AssetText canonical={d.quote_id} />
                    </td>
                    <td className="py-1.5 pr-4">
                      <code className="text-[11px]">{d.reference}</code>
                    </td>
                    <td className="py-1.5 pr-4 text-right font-mono tabular-nums">
                      {d.our_price}
                    </td>
                    <td className="py-1.5 pr-4 text-right font-mono tabular-nums">
                      {d.ref_price}
                    </td>
                    <td
                      className={`py-1.5 pr-4 text-right font-mono tabular-nums ${
                        firing ? 'text-down-strong' : 'text-ink-body'
                      }`}
                    >
                      {fmtDelta(d.delta_pct ?? '')}
                    </td>
                    <td className="text-ink-muted py-1.5 pr-4 font-mono text-[11px]">
                      {fmtTs(d.observed_at ?? '')}
                    </td>
                    <td className="py-1.5">
                      {firing ? (
                        <span className="bg-down-subtle text-down-strong rounded-sm px-1.5 py-0.5 text-[10px] font-medium uppercase">
                          firing
                        </span>
                      ) : (
                        <span className="bg-up-subtle text-up-strong rounded-sm px-1.5 py-0.5 text-[10px] font-medium uppercase">
                          clear
                        </span>
                      )}
                    </td>
                  </tr>
                );
              })}
            </tbody>
          </table>
        )}
      </Panel>
    </>
  );
}

// DivergenceSeriesPanel — the Δ% history for the selected (pair,
// reference), from GET /v1/divergence/series. The alert threshold the
// worker actually fires on is drawn as dashed ±threshold_pct reference
// lines; when the API serves no threshold, no band is drawn (never
// invented client-side). Points are last-observation-per-bucket at the
// resolution the API reports via bucket_seconds.
function DivergenceSeriesPanel({
  sel,
  days,
  onDays,
}: {
  sel: Selection | null;
  days: (typeof SERIES_WINDOWS)[number];
  onDays: (d: (typeof SERIES_WINDOWS)[number]) => void;
}) {
  const pair = sel ? `${sel.asset}~${sel.quote}` : '';
  const sq = useQuery<DivergenceSeriesResp>({
    queryKey: ['/v1/divergence/series', pair, sel?.reference, days],
    queryFn: async () => {
      const env = await apiGet<{ data: DivergenceSeriesResp }>(
        '/v1/divergence/series',
        {
          pair,
          reference: sel?.reference ?? '',
          days,
        },
      );
      return env.data;
    },
    enabled: sel != null,
    staleTime: 60_000,
    refetchInterval: 60_000,
  });

  const points = (sq.data?.points ?? [])
    .map((p) => ({
      time: Math.floor(Date.parse(p.t ?? '') / 1000),
      value: Number(p.delta_pct),
    }))
    .filter((p) => Number.isFinite(p.time) && Number.isFinite(p.value));
  const threshold = sq.data?.threshold_pct;
  const priceLines =
    threshold != null && threshold > 0
      ? [
          { value: threshold, label: `alert +${threshold}%` },
          { value: -threshold, label: `alert −${threshold}%` },
        ]
      : [];
  const bucketMin =
    sq.data?.bucket_seconds != null
      ? Math.round(sq.data.bucket_seconds / 60)
      : null;

  return (
    <Panel
      title={
        sel
          ? `Δ% history — ${shortAssetText(sel.asset)}/${shortAssetText(sel.quote)} vs ${sel.reference}`
          : 'Δ% history'
      }
      hint={
        bucketMin != null
          ? `Our VWAP vs the reference over the trailing window, one point per ${bucketMin} min (last observation per bucket). Dashed lines mark the operator's alert threshold.`
          : "Our VWAP vs the selected reference over the trailing window. Dashed lines mark the operator's alert threshold."
      }
      source={
        sel
          ? asExample('/v1/divergence/series', {
              pair,
              reference: sel.reference,
              days,
            })
          : undefined
      }
      bodyClassName="space-y-3"
    >
      <div className="flex gap-1">
        {SERIES_WINDOWS.map((d) => (
          <button
            key={d}
            onClick={() => onDays(d)}
            className={`rounded-md px-2.5 py-1 text-xs ${
              days === d
                ? 'bg-surface-strong text-ink'
                : 'border-line text-ink-body hover:border-brand-500 border'
            }`}
          >
            {d}d
          </button>
        ))}
      </div>
      {sel == null && (
        <p className="text-ink-muted text-sm">
          No (pair, reference) on the board yet — nothing to plot.
        </p>
      )}
      {sel != null && sq.isLoading && (
        <p className="text-ink-muted text-sm">Loading…</p>
      )}
      {sel != null && sq.isError && (
        <p className="text-ink-muted text-sm">
          The divergence history is unavailable right now.
        </p>
      )}
      {sel != null && sq.data && points.length === 0 && (
        <p className="text-ink-muted text-sm">
          No observations recorded for this pair × reference in the last{' '}
          {days === 1 ? 'day' : `${days} days`}.
        </p>
      )}
      {points.length > 0 && (
        <LineChart
          data={points}
          height={260}
          area={false}
          timeVisible={days === 1}
          priceLines={priceLines}
          legend={{
            valueLabel: 'Δ%',
            formatValue: (n) => `${n > 0 ? '+' : ''}${n.toFixed(3)}%`,
          }}
          ariaLabel={`Divergence of our VWAP vs ${sel?.reference} over the last ${days} day(s), in percent`}
        />
      )}
    </Panel>
  );
}

// BoardBars — the current board as signed horizontal bars, widest
// |Δ%| first. Bar length is magnitude; sign is carried by polarity
// color AND the signed value label (identity is never color-alone).
function BoardBars({ rows }: { rows: DivergenceResp['observations'] }) {
  const items = (rows ?? [])
    .slice(0, 12)
    .map((d) => {
      const n = Number(d.delta_pct);
      if (!Number.isFinite(n)) return null;
      const label = `${shortAssetText(d.asset_id)}/${shortAssetText(d.quote_id)} · ${d.reference}`;
      return {
        label,
        value: Math.abs(n),
        display: fmtDelta(d.delta_pct ?? ''),
        color: n >= 0 ? 'var(--color-up)' : 'var(--color-down)',
        annotation: d.status === 'firing' ? 'firing' : undefined,
        title: `${label}: ${fmtDelta(d.delta_pct ?? '')} (${d.status}) — click the matching board row to plot its history`,
      };
    })
    .filter((x): x is NonNullable<typeof x> => x != null);

  if (items.length === 0) return null;
  return (
    <Panel
      title="Current board by |Δ%|"
      hint="Every (pair, reference) currently on the board — bar length is the gap magnitude; green = we're above the reference, red = below."
      source={asExample('/v1/divergence', { limit: 100, window_days: 7 })}
    >
      <HBarList
        items={items}
        formatValue={(n) => `${n.toFixed(2)}%`}
        ariaLabel="Current divergence per (pair, reference), widest gap first, signed by direction"
      />
    </Panel>
  );
}
