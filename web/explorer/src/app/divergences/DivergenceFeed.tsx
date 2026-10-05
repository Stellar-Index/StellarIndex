'use client';

import { useState } from 'react';
import dynamic from 'next/dynamic';
import { useQuery } from '@tanstack/react-query';

import { Panel } from '@/components/reveal';
import { AssetText } from '@/components/AssetLink';
import { shortAssetText } from '@/lib/asset-label';
import { apiGet, asExample } from '@/api/client';
import { HBarList } from '@/components/charts/Bars';
import { CATEGORICAL_PALETTE } from '@/components/charts/DonutChart';
import { hueByIdentity } from '@/components/charts/dailyGaps';
import type { NamedLineSeries } from '@/components/charts/LineChart';
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

type Selection = { asset: string; quote: string };

type DivergencePair = NonNullable<DivergenceResp['pairs']>[number];

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

  const pairs = q.data?.pairs ?? [];
  // Default selection: the board's widest gap (the first pair — the
  // API orders pairs by their widest |Δ%| desc).
  const sel: Selection | null =
    selected ??
    (pairs[0]
      ? { asset: pairs[0].asset_id ?? '', quote: pairs[0].quote_id ?? '' }
      : null);

  return (
    <>
      <DivergenceSeriesPanel sel={sel} days={days} onDays={setDays} />

      <BoardBars pairs={pairs} />

      <Panel
        headingLevel={2}
        title="Divergence board"
        hint="Per pair, our VWAP beside the latest comparison against each external reference over the trailing 7 days, widest gap first. Choose a pair's Plot control to chart its history against every reference above."
        source={asExample('/v1/divergence', { limit: 100, window_days: 7 })}
        bodyClassName="space-y-3"
      >
        {q.isLoading && <p className="text-ink-muted text-sm">Loading…</p>}
        {q.isError && (
          <p className="text-ink-muted text-sm">
            The divergence board is unavailable right now.
          </p>
        )}
        {q.data && pairs.length === 0 && (
          <p className="text-ink-muted text-sm">
            No cross-reference comparisons recorded in the last 7 days (the
            divergence worker writes one row per configured (pair, reference)
            per tick).
          </p>
        )}
        {pairs.length > 0 && (
          // WCAG 1.4.10 Reflow: 8 columns of unbreakable mono cells scroll
          // inside the panel, not sideways across the whole page.
          <div className="overflow-x-auto">
            <table className="w-full text-sm">
              <thead>
                <tr className="border-line text-ink-muted border-b text-left text-[11px] tracking-wider uppercase">
                  <th scope="col" className="py-1.5 pr-4 font-normal">
                    Pair
                  </th>
                  <th
                    scope="col"
                    className="py-1.5 pr-4 text-right font-normal"
                  >
                    Our price
                  </th>
                  <th scope="col" className="py-1.5 pr-4 font-normal">
                    Reference
                  </th>
                  <th
                    scope="col"
                    className="py-1.5 pr-4 text-right font-normal"
                  >
                    Reference price
                  </th>
                  <th
                    scope="col"
                    className="py-1.5 pr-4 text-right font-normal"
                  >
                    Δ%
                  </th>
                  <th scope="col" className="py-1.5 pr-4 font-normal">
                    Observed
                  </th>
                  <th scope="col" className="py-1.5 pr-4 font-normal">
                    State
                  </th>
                  <th className="py-1.5 font-normal" aria-hidden />
                </tr>
              </thead>
              {pairs.map((p) => (
                <PairRows
                  key={`${p.asset_id}:${p.quote_id}`}
                  pair={p}
                  isSel={
                    sel != null &&
                    p.asset_id === sel.asset &&
                    p.quote_id === sel.quote
                  }
                  onSelect={() =>
                    setSelected({
                      asset: p.asset_id ?? '',
                      quote: p.quote_id ?? '',
                    })
                  }
                />
              ))}
            </table>
          </div>
        )}
      </Panel>
    </>
  );
}

// PairRows — one <tbody> per pair: the pair, our price and the Plot
// control span the pair's reference rows, so a reference's price is only
// ever read beside ours and the other references.
function PairRows({
  pair,
  isSel,
  onSelect,
}: {
  pair: DivergencePair;
  isSel: boolean;
  onSelect: () => void;
}) {
  const refs = pair.references ?? [];
  const span = Math.max(refs.length, 1);
  return (
    <tbody
      className={`border-line/60 hover:bg-surface-muted cursor-pointer border-b last:border-0 ${
        isSel ? 'bg-surface-muted' : ''
      }`}
    >
      {refs.map((r, i) => {
        const firing = r.status === 'firing';
        return (
          <tr key={r.reference} onClick={onSelect}>
            {i === 0 && (
              <>
                <td rowSpan={span} className="py-1.5 pr-4 align-top font-mono">
                  <AssetText canonical={pair.asset_id} />
                  <span className="text-ink-faint">/</span>
                  <AssetText canonical={pair.quote_id} />
                </td>
                <td
                  rowSpan={span}
                  className="py-1.5 pr-4 text-right align-top font-mono tabular-nums"
                >
                  {pair.our_price}
                </td>
              </>
            )}
            <td className="py-1.5 pr-4">
              <code className="text-[11px]">{r.reference}</code>
            </td>
            <td className="py-1.5 pr-4 text-right font-mono tabular-nums">
              {r.ref_price}
            </td>
            <td
              className={`py-1.5 pr-4 text-right font-mono tabular-nums ${
                firing ? 'text-down-strong' : 'text-ink-body'
              }`}
            >
              {fmtDelta(r.delta_pct ?? '')}
            </td>
            <td className="text-ink-muted py-1.5 pr-4 font-mono text-[11px]">
              {fmtTs(r.observed_at ?? pair.observed_at ?? '')}
            </td>
            <td className="py-1.5 pr-4">
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
            {/* The keyboard/AT path to the chart above. The rows' onClick
              is a mouse convenience only; this native <button> is what puts
              series selection in the tab order and gives Enter/Space
              activation for free (WCAG 2.1.1). The state is aria-current,
              NOT a toggle state: the board is single-select, so
              re-activating the plotted pair leaves it plotted. */}
            {i === 0 && (
              <td rowSpan={span} className="py-1.5 text-right align-top">
                <button
                  type="button"
                  aria-current={isSel ? 'true' : 'false'}
                  aria-label={`Plot ${shortAssetText(pair.asset_id)}/${shortAssetText(pair.quote_id)} divergence history`}
                  onClick={onSelect}
                  className={`focus-visible:ring-brand-500/60 rounded-sm px-1.5 py-0.5 text-[11px] transition-colors focus-visible:ring-2 focus-visible:outline-hidden ${
                    isSel
                      ? 'text-brand-600 font-medium'
                      : 'text-ink-faint hover:text-brand-600'
                  }`}
                >
                  {isSel ? 'Plotted' : 'Plot'}
                </button>
              </td>
            )}
          </tr>
        );
      })}
    </tbody>
  );
}

/**
 * One Δ% line per reference over the series' shared bucket axis. A
 * bucket a reference was not compared in is a gap (null), never joined
 * across. Hues are assigned by reference name so a window switch never
 * repaints a line.
 */
export function referenceLines(
  points: DivergenceSeriesResp['points'] | undefined,
): NamedLineSeries[] {
  const pts = points ?? [];
  const names = [
    ...new Set(
      pts.flatMap((p) => (p.references ?? []).map((r) => r.reference)),
    ),
  ].filter((n): n is NonNullable<typeof n> => n != null);
  const hue = hueByIdentity(names, CATEGORICAL_PALETTE);
  return names.map((name) => ({
    label: name,
    tone: 'brand' as const,
    color: hue.get(name),
    data: pts
      .map((p) => {
        const r = (p.references ?? []).find((x) => x.reference === name);
        const v = r ? Number(r.delta_pct) : NaN;
        return {
          time: Math.floor(Date.parse(p.t ?? '') / 1000),
          value: Number.isFinite(v) ? v : null,
        };
      })
      .filter((p) => Number.isFinite(p.time)),
  }));
}

// DivergenceSeriesPanel — the Δ% history for the selected pair against
// every reference, from GET /v1/divergence/series. The alert threshold the
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
    queryKey: ['/v1/divergence/series', pair, days],
    queryFn: async () => {
      const env = await apiGet<{ data: DivergenceSeriesResp }>(
        '/v1/divergence/series',
        { pair, days },
      );
      return env.data;
    },
    enabled: sel != null,
    staleTime: 60_000,
    refetchInterval: 60_000,
  });

  const lines = referenceLines(sq.data?.points).filter((l) =>
    l.data.some((p) => p.value != null),
  );
  const firingRefs = new Set<string>(
    (sq.data?.points ?? []).flatMap((p) =>
      (p.references ?? []).filter((r) => r.firing).map((r) => r.reference),
    ),
  );
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
      headingLevel={2}
      title={
        sel
          ? `Δ% history — ${shortAssetText(sel.asset)}/${shortAssetText(sel.quote)}`
          : 'Δ% history'
      }
      hint={
        bucketMin != null
          ? `Our VWAP vs every reference over the trailing window, one line per reference, one point per ${bucketMin} min (last observation per bucket). Dashed lines mark the operator's alert threshold.`
          : "Our VWAP vs every reference over the trailing window, one line per reference. Dashed lines mark the operator's alert threshold."
      }
      source={
        sel ? asExample('/v1/divergence/series', { pair, days }) : undefined
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
                ? 'bg-line text-ink'
                : 'border-line text-ink-body hover:border-brand-500 border'
            }`}
          >
            {d}d
          </button>
        ))}
      </div>
      {sel == null && (
        <p className="text-ink-muted text-sm">
          No pair on the board yet — nothing to plot.
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
      {sel != null && sq.data && lines.length === 0 && (
        <p className="text-ink-muted text-sm">
          No observations recorded for this pair in the last{' '}
          {days === 1 ? 'day' : `${days} days`}.
        </p>
      )}
      {lines.length > 0 && (
        <>
          <LineChart
            data={[]}
            series={lines}
            height={260}
            timeVisible={days === 1}
            priceLines={priceLines}
            legend={{
              valueLabel: 'Δ%',
              formatValue: (n) => `${n > 0 ? '+' : ''}${n.toFixed(3)}%`,
            }}
            ariaLabel={`Divergence of our VWAP vs ${lines.map((l) => l.label).join(', ')} over the last ${days} day(s), in percent`}
          />
          {/* Static legend: line identity never rests on colour alone. */}
          <ul className="flex flex-wrap gap-x-4 gap-y-1.5">
            {lines.map((l) => (
              <li key={l.label} className="flex items-center gap-1.5">
                <span
                  aria-hidden
                  className="h-0.5 w-3 rounded-full"
                  style={{ backgroundColor: l.color }}
                />
                <span className="text-ink-body text-xs">
                  {l.label}
                  {firingRefs.has(l.label) && (
                    <span className="text-down-strong"> · fired</span>
                  )}
                </span>
              </li>
            ))}
          </ul>
        </>
      )}
    </Panel>
  );
}

// BoardBars — the current board as signed horizontal bars, widest
// |Δ%| first. Bar length is magnitude; sign is carried by polarity
// color AND the signed value label (identity is never color-alone).
function BoardBars({ pairs }: { pairs: DivergencePair[] }) {
  const items = pairs
    .flatMap((p) =>
      (p.references ?? []).map((r) => {
        const n = Number(r.delta_pct);
        if (!Number.isFinite(n)) return null;
        const label = `${shortAssetText(p.asset_id)}/${shortAssetText(p.quote_id)} · ${r.reference}`;
        return {
          label,
          value: Math.abs(n),
          display: fmtDelta(r.delta_pct ?? ''),
          color: n >= 0 ? 'var(--color-up)' : 'var(--color-down)',
          annotation: r.status === 'firing' ? 'firing' : undefined,
          title: `${label}: ${fmtDelta(r.delta_pct ?? '')} (${r.status}) — choose the pair's Plot control to chart its history`,
        };
      }),
    )
    .filter((x): x is NonNullable<typeof x> => x != null)
    .sort((a, b) => b.value - a.value)
    .slice(0, 12);

  if (items.length === 0) return null;
  return (
    <Panel
      headingLevel={2}
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
