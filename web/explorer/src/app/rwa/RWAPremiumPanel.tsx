'use client';

import { useMemo, useState } from 'react';
import dynamic from 'next/dynamic';
import { useQuery } from '@tanstack/react-query';

import { Panel } from '@/components/reveal';
import { apiGetData, asExample } from '@/api/client';
import type { components } from '@/api/types';
import { CATEGORICAL_PALETTE } from '@/components/charts/DonutChart';
import { hueByIdentity, toDailyLine } from '@/components/charts/dailyGaps';
import { truncateMiddle } from '@/components/ui/Mono';
import { Callout, EmptyState, Segmented, Skeleton } from '@/components/ui';
import type { NamedLineSeries } from '@/components/charts/LineChart';

type Schemas = components['schemas'];
type RWAPremiumHistoryView = Schemas['RWAPremiumHistoryView'];
type RWAPremiumSeries = Schemas['RWAPremiumSeries'];

const ENDPOINT = '/v1/rwa/premium';

// The chart engine is client-only (canvas). Loaded the way every other
// chart on the explorer loads it, so the panel's shell renders on the
// server and only the plot is deferred.
const LineChart = dynamic(
  () => import('@/components/charts/LineChart').then((m) => m.LineChart),
  { ssr: false, loading: () => <Skeleton className="h-[300px] w-full" /> },
);

/**
 * How many instruments the chart draws before folding the tail.
 *
 * Bounded by the categorical palette, whose hues are assigned in FIXED
 * order and never cycled — a tenth series repainted in the first hue
 * would read as the first instrument. The tail is not averaged into an
 * "Other" line: a premium is a percentage of a different denominator
 * for every instrument, so there is no honest way to combine two of
 * them into one curve.
 */
const MAX_LINES = 8;

/**
 * The zero line's colour: a neutral that is neither the up nor the down
 * tone. Zero is not good news or bad news here, it is the axis the
 * finding is measured from, and painting it red or green would state a
 * verdict the data does not carry.
 */
const ZERO_LINE_COLOR = '#5b6472';

const TIMEFRAMES = [
  { label: '1M', value: '1mo' },
  { label: '1Y', value: '1y' },
  { label: 'All', value: 'all' },
];

function useRWAPremium(timeframe: string) {
  return useQuery<RWAPremiumHistoryView>({
    queryKey: [ENDPOINT, timeframe],
    queryFn: () => apiGetData<RWAPremiumHistoryView>(ENDPOINT, { timeframe }),
    staleTime: 300_000,
    placeholderData: (prev) => prev,
  });
}

function pct(n: number): string {
  return `${n >= 0 ? '+' : ''}${n.toFixed(2)}%`;
}

/**
 * Per-instrument lines, drawn in the server's order (widest absolute
 * premium first) but COLOURED BY IDENTITY, never by rank — see
 * [hueByIdentity]. Missing days become gap points so the line breaks
 * rather than being drawn across a day nothing was observed on.
 */
export function premiumLines(series: RWAPremiumSeries[]): NamedLineSeries[] {
  const drawn = series.slice(0, MAX_LINES);
  const hue = hueByIdentity(
    drawn.map((s) => s.asset_id),
    CATEGORICAL_PALETTE,
  );
  return drawn.flatMap((s) => {
    const data = toDailyLine(s.points, (p) => Number(p.premium_pct));
    if (data.length === 0) return [];
    return [
      {
        label: s.code || s.label || truncateMiddle(s.asset_id, 6, 6),
        data,
        tone: 'brand' as const,
        color: hue.get(s.asset_id),
      },
    ];
  });
}

/**
 * RWAPremiumPanel — what the market pays for a tokenized instrument
 * against what an independent oracle says the instrument is worth.
 *
 * This is the chart a chain-query tool structurally cannot draw: it
 * needs both an observed market price and an oracle's published value
 * on the same clock, and a chain query has neither leg. It is also, by
 * construction, mostly holes — most of this set is bought and held, so
 * the instruments have a published value every day and a market price
 * on very few. The strip below the plot is where that lives.
 */
export function RWAPremiumPanel() {
  const [timeframe, setTimeframe] = useState('1y');
  const { data, isLoading, isError, error } = useRWAPremium(timeframe);

  const lines = useMemo(() => premiumLines(data?.series ?? []), [data?.series]);

  return (
    <Panel
      title="Premium and discount to net asset value"
      headingLevel={2}
      hint="daily · market price against the oracle's valuation"
      source={asExample(ENDPOINT, { timeframe })}
      bodyClassName="space-y-3"
    >
      <Segmented
        options={TIMEFRAMES}
        value={timeframe}
        onChange={setTimeframe}
        ariaLabel="Chart window"
      />
      {isError || (!data && !isLoading) ? (
        <Callout tone="bad" title="Failed to load the premium history">
          {error instanceof Error
            ? error.message
            : 'The request did not complete.'}
        </Callout>
      ) : isLoading && !data ? (
        <Skeleton className="h-[300px] w-full" />
      ) : (
        <PremiumBody data={data as RWAPremiumHistoryView} lines={lines} />
      )}
    </Panel>
  );
}

function PremiumBody({
  data,
  lines,
}: {
  data: RWAPremiumHistoryView;
  lines: NamedLineSeries[];
}) {
  if (lines.length === 0) {
    return (
      <EmptyState
        headingLevel={3}
        title="No premium series is published"
        description={data.basis}
      />
    );
  }
  return (
    <>
      <LineChart
        data={[]}
        series={lines}
        height={300}
        area={false}
        ariaLabel={premiumAriaLabel(data, lines)}
        legend={{ valueLabel: 'Premium', formatValue: pct }}
        // Zero is the finding. Without it drawn, a reader has to read
        // the axis to learn which side of par a line is on.
        priceLines={[{ value: 0, label: 'par', color: ZERO_LINE_COLOR }]}
      />
      <Legend lines={lines} />
      <Coverage data={data} />
    </>
  );
}

/**
 * The static legend. Always present, so identity never rests on colour
 * alone — the crosshair legend is a hover affordance, not an
 * alternative to this. With a single line it also does the work a title
 * cannot: it names WHICH instrument the one curve is.
 */
function Legend({ lines }: { lines: NamedLineSeries[] }) {
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

/**
 * The coverage strip. Not a footnote: this chart draws one line for a
 * handful of the set's members, and a reader who took it for the state
 * of the sector would be reading something nobody published.
 */
function Coverage({ data }: { data: RWAPremiumHistoryView }) {
  const shown = Math.min(data.series.length, MAX_LINES);
  const hidden = data.series.length - shown;
  const latest = data.coverage?.[data.coverage.length - 1];
  const withheld = data.series.reduce(
    (n, s) => n + (s.market_withheld_days ?? 0),
    0,
  );
  return (
    <div className="text-ink-muted space-y-1.5 text-xs leading-relaxed">
      <p>
        <strong className="text-ink-body">{`${data.members} of ${data.assets}`}</strong>{' '}
        {`assets in the set can be compared at all: ${data.bound} carry a curated binding to an instrument feed, and only some of those trade against a dollar.`}{' '}
        {latest
          ? `On the most recent day with any measurement, ${latest.assets_measured} of ${data.assets} carried a premium. `
          : ''}
        A day either the market or the oracle was silent on is a break in the
        line, never a flat stretch — neither leg may be carried across a day it
        was not observed on.
      </p>
      {withheld > 0 && (
        <p>
          {`${withheld} day${withheld === 1 ? '' : 's'} carried observed trades that did not clear the thin-market floor and are absent from the lines.`}{' '}
          A price claim needs a market with substance behind it; honest low
          volume stays visible through the raw trade surfaces.
        </p>
      )}
      {data.excluded && data.excluded.length > 0 && (
        <p>
          Left out entirely:{' '}
          {data.excluded
            .map((e) => `${e.assets} ${excludedLabel(e.reason)}`)
            .join(', ')}
          .
        </p>
      )}
      {hidden > 0 && (
        <p>
          Showing the {shown} widest; {hidden} narrower
          {hidden === 1 ? ' one is' : ' ones are'} not drawn — they are not
          averaged into an &ldquo;other&rdquo; line, because a premium is a
          percentage of a different denominator for every instrument and the set
          has no mean anyone published.
        </p>
      )}
      <p>
        Valued against {data.sources?.join(', ') || 'an independent oracle'}
        &rsquo;s published value for each instrument. The correspondence between
        one token and one unit of that instrument is the issuer&rsquo;s own
        declaration, not an independent measurement.
      </p>
    </div>
  );
}

const EXCLUDED_LABEL: Record<string, string> = {
  not_bound: 'with no oracle feed bound to their exact (code, issuer)',
  contract_not_bound: 'contract-issued, which the binding table cannot key',
  issuer_flagged: 'withheld for a scam-class issuer flag',
  no_reference_history: 'whose instrument has no published value history',
  no_market_history: 'that have never traded against a dollar',
  market_below_floor: 'whose market never cleared the thin-market floor',
  no_same_day_observation:
    'whose market and oracle were never observed on the same day',
};

function excludedLabel(reason: string): string {
  return EXCLUDED_LABEL[reason] ?? reason;
}

function premiumAriaLabel(
  data: RWAPremiumHistoryView,
  lines: NamedLineSeries[],
): string {
  const latest = data.coverage?.[data.coverage.length - 1];
  return (
    `Premium and discount to net asset value, ${lines.length} daily series: ` +
    `${lines.map((l) => l.label).join(', ')}. ` +
    `${latest ? `${latest.assets_measured} of ${data.assets}` : 'None'} of the set ` +
    `carried a premium on the most recent measured day. Positive is a premium ` +
    `over the instrument's published value, negative a discount.`
  );
}
