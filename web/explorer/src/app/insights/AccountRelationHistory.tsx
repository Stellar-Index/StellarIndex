'use client';

import { useState } from 'react';
import { useQuery } from '@tanstack/react-query';

import { Panel } from '@/components/reveal';
import { LineChart } from '@/components/charts/LineChart';
import { Callout, Segmented, Stat } from '@/components/ui';
import { apiGet, asExample, type Envelope } from '@/api/client';
import { formatCompact } from '@/lib/format';
import type { components } from '@/api/types';

import { formatTimestamp } from '../explorer-shared';
import {
  cumulativeLine,
  errorStatus,
  monthlyLine,
  RELATION,
  type HistorySeries,
  type Relation,
} from './accountRelation';

type AccountGraphHistoryResp = components['schemas']['AccountGraphHistory'];

type Metric = 'new' | 'events' | 'cumulative';

const numFmt = new Intl.NumberFormat('en-US');

/**
 * AccountRelationHistory — the time axis of one address's graph, over
 * GET /v1/accounts/{g}/graph/history.
 *
 * Three things this panel is built to never do quietly.
 *
 * IT NEVER DRAWS THROUGH A QUIET MONTH. The endpoint emits no point for
 * a month with no activity, so the monthly metrics break the line there
 * instead of joining the two sides at a value nobody served. The one
 * exception is the running total, which crosses a quiet month unbroken
 * because a total that gained nothing has not become unknown — the copy
 * under the chart says which of the two a reader is looking at.
 *
 * IT NEVER PRESENTS `events` AS A TOTAL. Events between the first and
 * last of a repeated relationship have no recorded month, so each
 * point's `events` is a LOWER BOUND whenever the series says so. The
 * chart is labelled accordingly and the shortfall is printed, by reason,
 * from the payload's own `unplaced[]` register rather than dropped.
 *
 * IT NEVER CALLS A MISSING ENDPOINT A WARMING ONE. `graph/history`
 * postdates the deployed API on some environments; a 404 there says the
 * API is older than the explorer, which retrying does not fix.
 */
export function AccountRelationHistory({
  account,
  relation,
}: {
  account: string;
  relation: Relation;
}) {
  const [metric, setMetric] = useState<Metric>('new');
  const vocabulary = RELATION[relation];
  const path = `/v1/accounts/${account}/graph/history`;
  const source = asExample(path);

  const { data, isLoading, isError, error } = useQuery<AccountGraphHistoryResp>(
    {
      queryKey: ['/v1/accounts/{id}/graph/history', account],
      enabled: account.length > 0,
      retry: false,
      staleTime: 5 * 60_000,
      queryFn: async () => {
        const env = await apiGet<Envelope<AccountGraphHistoryResp>>(
          `/v1/accounts/${encodeURIComponent(account)}/graph/history`,
        );
        return env.data;
      },
    },
  );

  const title = 'Change over time';
  const hint = `monthly ${vocabulary.counterparties}, from the graph on a time axis`;

  if (isLoading) {
    return (
      <Panel
        headingLevel={2}
        title={title}
        hint={hint}
        source={source}
        bodyClassName="text-sm text-ink-muted"
      >
        Loading the monthly series…
      </Panel>
    );
  }

  if (isError || !data) {
    const status = errorStatus(error);
    return (
      <Panel
        headingLevel={2}
        title={title}
        hint={hint}
        source={source}
        bodyClassName="text-sm text-ink-muted"
      >
        {status === 404 ? (
          <p>
            This deployment&apos;s API does not serve{' '}
            <code className="font-mono text-xs">{path}</code> yet. The monthly
            series is newer than the API build behind this explorer, so it is
            absent rather than empty — retrying will not produce it; the API has
            to ship first. Every other panel on this page is unaffected.
          </p>
        ) : status === 503 ? (
          <p>
            The monthly series is warming — it is rebuilt by a rollup cycle, and
            this deployment has not completed one yet.
          </p>
        ) : (
          <p>
            The monthly series could not be read
            {error instanceof Error ? `: ${error.message}` : ''}.
          </p>
        )}
      </Panel>
    );
  }

  const series: HistorySeries = data.series[relation];
  const totals = series.totals;

  if (series.points.length === 0) {
    return (
      <Panel
        headingLevel={2}
        title={title}
        hint={hint}
        source={source}
        bodyClassName="space-y-2 text-sm text-ink-muted"
      >
        <p>
          No month in the covered span carries a {vocabulary.eventNoun} for this
          address.
        </p>
        <CoverageStrip series={series} />
      </Panel>
    );
  }

  // The event arm is the only one that can be a lower bound, so the
  // switcher labels it as one rather than leaving the caveat to a
  // footnote the reader meets after reading the line.
  const eventsLabel = series.lower_bound
    ? `${vocabulary.eventColumn} (lower bound)`
    : vocabulary.eventColumn;

  const line =
    metric === 'cumulative'
      ? cumulativeLine(series.points, (p) => p.new_accounts)
      : monthlyLine(
          series.points,
          metric === 'new' ? (p) => p.new_accounts : (p) => p.events,
        );

  const valueLabel =
    metric === 'new'
      ? 'New accounts'
      : metric === 'events'
        ? eventsLabel
        : 'Accounts, cumulative';

  return (
    <Panel
      headingLevel={2}
      title={title}
      hint={hint}
      source={source}
      bodyClassName="space-y-4"
    >
      <div className="flex flex-wrap items-center justify-between gap-2">
        <p className="text-ink-muted text-xs">
          Calendar months, UTC. <strong>New accounts</strong> counts
          counterparties reached for the FIRST time that month and is exact.{' '}
          <strong>{vocabulary.eventColumn}</strong> counts the individual
          operations.
        </p>
        <Segmented
          ariaLabel="History metric"
          value={metric}
          onChange={(v) => setMetric(v as Metric)}
          options={[
            { label: 'New accounts', value: 'new' },
            { label: eventsLabel, value: 'events' },
            { label: 'Cumulative', value: 'cumulative' },
          ]}
        />
      </div>

      {/* tone="info", not "warn": a lower bound is a standing property of
          the measure, not an error event, and the warn tone carries
          role="alert" + aria-live="assertive", which would interrupt a
          screen reader on every toggle of the metric switch. */}
      {metric === 'events' && series.lower_bound && (
        <Callout tone="info" title="This line is a lower bound">
          <p>
            An edge stores only its first and last timestamp, so operations
            between the first and the last of a repeated relationship have no
            recorded month. They are counted in the totals below and are{' '}
            <strong>not</strong> spread across the months — so every point on
            this line is at least its drawn value, and the whole-history figure
            is the one to compare against.
          </p>
        </Callout>
      )}

      <LineChart
        data={line}
        height={260}
        area={metric === 'cumulative'}
        positive
        ariaLabel={`Monthly ${valueLabel.toLowerCase()} for ${vocabulary.actor} ${account}`}
        legend={{
          valueLabel,
          formatValue: (n) => formatCompact(n),
        }}
      />

      <p className="text-ink-faint text-[11px]">
        {metric === 'cumulative' ? (
          <>
            A running total of new accounts. It crosses a month with no activity
            unbroken, and that is not an interpolation: the monthly counts are
            exact and sum to the whole-history total, so a month that added
            nothing leaves the total exactly where it was.
          </>
        ) : (
          <>
            A month with no activity emits no point, so the line{' '}
            <strong>breaks</strong> there rather than being drawn through a zero
            nobody reported. Inside the coverage span below, a break means
            nothing happened; outside it, nothing was observed.
          </>
        )}
      </p>

      <dl className="grid grid-cols-2 gap-x-6 gap-y-3 sm:grid-cols-4">
        <Stat
          label="Accounts, all time"
          value={numFmt.format(totals.accounts)}
        />
        <Stat
          label={`${vocabulary.eventColumn}, all time`}
          value={numFmt.format(totals.events)}
        />
        <Stat
          label="Placed in a month"
          value={numFmt.format(totals.events_placed)}
        />
        <Stat
          label="Not placeable"
          value={numFmt.format(totals.events_unplaced)}
        />
      </dl>

      {/* The payload's own register of what it could not place. Printing
          it is the difference between a smaller claim and a smaller claim
          wearing the full one's name. */}
      {series.unplaced && series.unplaced.length > 0 && (
        <div className="border-line rounded-md border p-3">
          <p className="text-ink-muted text-[11px] tracking-wider uppercase">
            Events without a month
          </p>
          <ul className="mt-2 space-y-2">
            {series.unplaced.map((u) => (
              <li key={u.reason} className="text-xs">
                <span className="text-ink-body font-mono">{u.reason}</span>{' '}
                <span className="tnum text-ink">
                  — {numFmt.format(u.events)}
                </span>
                <p className="text-ink-muted mt-0.5">{u.detail}</p>
              </li>
            ))}
          </ul>
        </div>
      )}

      <CoverageStrip series={series} />
      <p className="text-ink-faint text-[11px]">{data.note}</p>
    </Panel>
  );
}

function CoverageStrip({ series }: { series: HistorySeries }) {
  const c = series.coverage;
  return (
    <p className="text-ink-faint text-[11px]">
      Covers ledgers{' '}
      <span className="tnum">{numFmt.format(c.from_ledger)}</span>–
      <span className="tnum">{numFmt.format(c.thru_ledger)}</span> (
      {formatTimestamp(c.from_time)} to {formatTimestamp(c.thru_time)}) — the
      span the rollup behind this arm actually aggregated, not a claim about the
      chain. Snapshot computed {formatTimestamp(c.computed_at)}.
    </p>
  );
}
