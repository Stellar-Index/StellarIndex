'use client';

import { useMemo, useState } from 'react';
import dynamic from 'next/dynamic';
import { useQuery } from '@tanstack/react-query';

import { Panel } from '@/components/reveal';
import { apiGetData, asExample } from '@/api/client';
import type { components } from '@/api/types';
import { formatCompact } from '@/lib/format';
import { truncateMiddle } from '@/components/ui/Mono';
import { CATEGORICAL_PALETTE } from '@/components/charts/DonutChart';
import { Callout, EmptyState, Segmented, Skeleton } from '@/components/ui';
import type { NamedLineSeries } from '@/components/charts/LineChart';

type Schemas = components['schemas'];
type RWAHistoryView = Schemas['RWAHistoryView'];
type RWAHistoryPoint = Schemas['RWAHistoryPoint'];
type RWAHistoryGroup = Schemas['RWAHistoryGroup'];

const ENDPOINT = '/v1/rwa/history';

// The chart engine is client-only (canvas). Loaded the way every other
// chart on the explorer loads it, so the panel's shell renders on the
// server and only the plot is deferred.
const LineChart = dynamic(
  () => import('@/components/charts/LineChart').then((m) => m.LineChart),
  { ssr: false, loading: () => <Skeleton className="h-[300px] w-full" /> },
);

/**
 * How many constituents the by-asset view draws before folding the tail.
 *
 * Bounded by the categorical palette, whose hues are assigned in FIXED
 * order and never cycled — a tenth series repainted in the first hue
 * would read as the first asset. The tail is not summed into an "Other"
 * line: adding heterogeneous instruments into one curve would invent a
 * series nobody publishes. It is simply named, with its count.
 */
const MAX_ASSET_LINES = 8;

const TIMEFRAMES = [
  { label: '1M', value: '1mo' },
  { label: '1Y', value: '1y' },
  { label: 'All', value: 'all' },
];

const VIEWS = [
  { label: 'Total', value: 'total' },
  { label: 'By asset', value: 'asset' },
];

function useRWAHistory(timeframe: string, groupBy: string) {
  return useQuery<RWAHistoryView>({
    queryKey: [ENDPOINT, timeframe, groupBy],
    queryFn: () =>
      apiGetData<RWAHistoryView>(ENDPOINT, { timeframe, group_by: groupBy }),
    staleTime: 300_000,
    placeholderData: (prev) => prev,
  });
}

/** Unix seconds for a served RFC 3339 day, or null when unparsable. */
function pointTime(t: string): number | null {
  const ms = Date.parse(t);
  return Number.isFinite(ms) ? Math.floor(ms / 1000) : null;
}

/**
 * Served points → chart geometry.
 *
 * `value` is parsed to a JS number for the y-coordinate ONLY. The wire
 * carries exact decimal strings (ADR-0003) and every figure the panel
 * PRINTS comes from those; a pixel is allowed to be a float.
 *
 * `volume` carries the day's `assets_valued` into the pane below the
 * line. It is not a second y-axis on the same plot — lightweight-charts
 * gives it its own pane and its own scale — and it is the whole reason
 * this chart can be read honestly: a fall in the line that coincides
 * with a fall in coverage is a fall in what we could see, not in what
 * the sector is worth.
 */
function toLine(points: RWAHistoryPoint[], withCoverage: boolean) {
  return points.flatMap((p) => {
    const time = pointTime(p.t);
    if (time == null) return [];
    return [
      {
        time,
        value: Number(p.value_usd),
        ...(withCoverage ? { volume: p.assets_valued } : {}),
      },
    ];
  });
}

function usdCompact(n: number): string {
  return `$${formatCompact(n)}`;
}

/**
 * RWAHistoryPanel — the set's value over time, on the reference basis.
 *
 * This is the panel the /rwa page was missing: everything else on it is
 * a snapshot. It is deliberately NOT a market-cap chart — most of this
 * set is held rather than traded — and it is deliberately not drawn as
 * a clean unbroken line, because it is not one. See the coverage strip
 * below the plot.
 */
export function RWAHistoryPanel() {
  const [timeframe, setTimeframe] = useState('1y');
  const [view, setView] = useState('total');
  const groupBy = view === 'asset' ? 'asset' : 'none';
  const { data, isLoading, isError, error } = useRWAHistory(timeframe, groupBy);

  const lines = useMemo<NamedLineSeries[]>(
    () => assetLines(data?.groups ?? []),
    [data?.groups],
  );
  const total = useMemo(() => toLine(data?.points ?? [], true), [data?.points]);

  const controls = (
    <div className="flex flex-wrap items-center gap-2">
      <Segmented
        options={VIEWS}
        value={view}
        onChange={setView}
        ariaLabel="Series decomposition"
      />
      <Segmented
        options={TIMEFRAMES}
        value={timeframe}
        onChange={setTimeframe}
        ariaLabel="Chart window"
      />
    </div>
  );

  return (
    <Panel
      title="Value of backing over time"
      headingLevel={2}
      hint="daily · what oracles say the instruments are worth"
      source={asExample(ENDPOINT, { timeframe, group_by: groupBy })}
      bodyClassName="space-y-3"
    >
      {controls}
      {isError || (!data && !isLoading) ? (
        <Callout tone="bad" title="Failed to load the value history">
          {error instanceof Error
            ? error.message
            : 'The request did not complete.'}
        </Callout>
      ) : isLoading && !data ? (
        <Skeleton className="h-[300px] w-full" />
      ) : (
        <HistoryBody
          data={data as RWAHistoryView}
          view={view}
          total={total}
          lines={lines}
        />
      )}
    </Panel>
  );
}

function HistoryBody({
  data,
  view,
  total,
  lines,
}: {
  data: RWAHistoryView;
  view: string;
  total: { time: number; value: number; volume?: number }[];
  lines: NamedLineSeries[];
}) {
  if (data.points.length === 0) {
    return (
      <EmptyState
        headingLevel={3}
        title="No value series is published"
        description={data.basis}
      />
    );
  }
  const last = data.points[data.points.length - 1];
  const multi = view === 'asset' && lines.length > 0;
  return (
    <>
      <WindowChange points={data.points} />
      {multi ? (
        <LineChart
          data={[]}
          series={lines}
          height={300}
          ariaLabel={assetAriaLabel(data, lines)}
          legend={{ valueLabel: 'Value', formatValue: usdCompact }}
        />
      ) : (
        <LineChart
          data={total}
          height={300}
          area
          positive
          ariaLabel={totalAriaLabel(data)}
          legend={{
            valueLabel: 'Value of backing',
            volumeLabel: 'Assets valued',
            formatValue: usdCompact,
            formatVolume: (n) => `${n} of ${data.assets}`,
          }}
        />
      )}
      {multi && <AssetLegend lines={lines} />}
      <Coverage data={data} last={last} multi={multi} />
    </>
  );
}

/**
 * Change across the window — and a refusal to state one when the two
 * endpoints do not cover the same number of assets.
 *
 * This is the misreading the whole surface is built to prevent. A series
 * whose first point summed six assets and whose last point summed nine
 * has "grown" by an amount that is partly the sector and partly our
 * coverage of it, and there is no way to say which from the two figures.
 * The number is withheld and the reason is named, exactly as a withheld
 * market cap is.
 */
function WindowChange({ points }: { points: RWAHistoryPoint[] }) {
  if (points.length < 2) return null;
  const first = points[0];
  const last = points[points.length - 1];
  if (first.assets_valued !== last.assets_valued) {
    return (
      <p className="text-ink-muted text-xs leading-relaxed">
        <strong className="text-ink-body">No change figure.</strong> The window
        opens with {first.assets_valued} asset
        {first.assets_valued === 1 ? '' : 's'} valued and closes with{' '}
        {last.assets_valued}, so the two ends are sums over different sets and
        their difference is not growth.
      </p>
    );
  }
  const from = Number(first.value_usd);
  const to = Number(last.value_usd);
  if (!(from > 0) || !Number.isFinite(to)) return null;
  const pct = ((to - from) / from) * 100;
  const up = pct >= 0;
  return (
    <p className="text-ink-muted text-xs leading-relaxed">
      <span className={up ? 'text-up' : 'text-down'}>
        {up ? '+' : ''}
        {pct.toFixed(1)}%
      </span>{' '}
      across the window, over a constant {first.assets_valued} valued asset
      {first.assets_valued === 1 ? '' : 's'} — {usdCompact(from)} →{' '}
      {usdCompact(to)}.
    </p>
  );
}

/**
 * The coverage strip. It is not a footnote: on most days this total is a
 * floor, and a reader who took the line for the value of the sector
 * would be reading a number nobody published.
 */
function Coverage({
  data,
  last,
  multi,
}: {
  data: RWAHistoryView;
  last: RWAHistoryPoint;
  multi: boolean;
}) {
  const shown = Math.min(data.groups?.length ?? 0, MAX_ASSET_LINES);
  const hidden = (data.groups?.length ?? 0) - shown;
  return (
    <div className="text-ink-muted space-y-1.5 text-xs leading-relaxed">
      <p>
        {last.lower_bound ? (
          <>
            <strong className="text-ink-body">A floor, not a total.</strong>{' '}
            {last.assets_valued} of {data.assets} assets in the set carried both
            a supply history and an oracle-published value on the most recent
            day; the other {last.assets_unvalued} contribute nothing to the
            line.
          </>
        ) : (
          <>
            <strong className="text-ink-body">Full coverage.</strong> Every one
            of the {data.assets} assets in the set was valued on the most recent
            day.
          </>
        )}{' '}
        A day on which no member could be valued is a gap in the line rather
        than a zero.
      </p>
      {data.excluded && data.excluded.length > 0 && (
        <p>
          Left out of the series:{' '}
          {data.excluded
            .map((e) => `${e.assets} ${excludedLabel(e.reason)}`)
            .join(', ')}
          .
        </p>
      )}
      {multi && hidden > 0 && (
        <p>
          Showing the {shown} largest constituents; {hidden} smaller
          {hidden === 1 ? ' one is' : ' ones are'} in the total but not drawn —
          they are not folded into an &ldquo;other&rdquo; line, because adding
          unlike instruments into one curve would invent a series nobody
          publishes.
        </p>
      )}
      <p>
        Valued at {data.sources?.join(', ') || 'an independent oracle'}
        &rsquo;s published value for each instrument, times the tokens in
        circulation. Not a market capitalisation — nobody was observed paying
        it. Membership is today&rsquo;s set applied backwards.
      </p>
    </div>
  );
}

const EXCLUDED_LABEL: Record<string, string> = {
  not_bound: 'with no oracle feed bound to their exact (code, issuer)',
  contract_not_bound: 'contract-issued, which the binding table cannot key',
  issuer_flagged: 'withheld for a scam-class issuer flag',
  no_supply_history: 'with no supply history in the lake',
  supply_incomplete: 'whose recorded supply flows are incomplete',
  no_reference_history: 'whose instrument has no published value history',
};

function excludedLabel(reason: string): string {
  return EXCLUDED_LABEL[reason] ?? reason;
}

/**
 * Per-asset lines, coloured from the shared categorical palette in the
 * order the server ranked them (largest last value first).
 *
 * The hue follows the ENTITY's rank in a stable server-side order, and
 * the tail is dropped rather than cycled back onto the first hue.
 */
function assetLines(groups: RWAHistoryGroup[]): NamedLineSeries[] {
  return groups.slice(0, MAX_ASSET_LINES).flatMap((g, i) => {
    const data = toLine(g.points, false);
    if (data.length === 0) return [];
    return [
      {
        label: g.code || g.label || truncateMiddle(g.key, 6, 6),
        data,
        tone: 'brand' as const,
        color: CATEGORICAL_PALETTE[i],
      },
    ];
  });
}

/**
 * The static legend. Present whenever more than one line is drawn, so
 * identity never rests on colour alone (the crosshair legend is a hover
 * affordance, not an alternative to this).
 */
function AssetLegend({ lines }: { lines: NamedLineSeries[] }) {
  return (
    <ul className="flex flex-wrap gap-x-4 gap-y-1.5">
      {lines.map((l) => (
        <li key={l.label} className="flex items-center gap-1.5">
          <span
            aria-hidden
            className="h-0.5 w-3 rounded-full"
            style={{ backgroundColor: l.color }}
          />
          <span className="text-ink-body text-xs">{l.label}</span>
        </li>
      ))}
    </ul>
  );
}

function totalAriaLabel(data: RWAHistoryView): string {
  const last = data.points[data.points.length - 1];
  return `Value of backing for the real-world asset set, ${data.points.length} daily points. Most recent: ${usdCompact(Number(last.value_usd))} across ${last.assets_valued} of ${data.assets} assets${last.lower_bound ? ', a lower bound' : ''}.`;
}

function assetAriaLabel(
  data: RWAHistoryView,
  lines: NamedLineSeries[],
): string {
  return `Value of backing by asset, ${lines.length} series over ${data.points.length} daily points: ${lines.map((l) => l.label).join(', ')}.`;
}
