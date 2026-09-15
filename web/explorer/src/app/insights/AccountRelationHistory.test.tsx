import { describe, it, expect, beforeEach, vi } from 'vitest';
import { fireEvent, render, screen } from '@testing-library/react';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';

import { AccountRelationHistory } from './AccountRelationHistory';

const ACCOUNT = 'GDB3RSSWTUXO7MBTNMHUP3DRBIUR3QRV2CVFRAKMN4GM2B4QNGEUT6CU';

const apiGet = vi.hoisted(() => vi.fn());
vi.mock('@/api/client', async (importOriginal) => {
  const mod = await importOriginal<typeof import('@/api/client')>();
  return { ...mod, apiGet };
});

// The chart is a canvas; what matters for these guards is the GEOMETRY
// handed to it, so the stub publishes its points back to the DOM.
vi.mock('@/components/charts/LineChart', () => ({
  LineChart: (props: {
    data: { time: number; value: number | null }[];
    ariaLabel?: string;
  }) => (
    <div
      data-testid="line-chart"
      data-points={JSON.stringify(props.data)}
      aria-label={props.ariaLabel}
    />
  ),
}));

const coverage = {
  from_ledger: 32_747_295,
  thru_ledger: 64_428_050,
  from_time: '2020-11-23T16:00:18Z',
  thru_time: '2026-09-14T17:45:39Z',
  computed_at: '2026-09-14T17:57:59Z',
};

// January and April carry activity; February and March are QUIET, which
// the endpoint expresses by emitting no point at all for them.
const history = {
  data: {
    account: ACCOUNT,
    granularity: '1M',
    series: {
      created: {
        points: [],
        totals: {
          accounts: 0,
          events: 0,
          events_placed: 0,
          events_unplaced: 0,
        },
        lower_bound: false,
        coverage,
      },
      sponsored: {
        points: [
          {
            period: '2024-01',
            period_start: '2024-01-01T00:00:00Z',
            new_accounts: 4,
            events: 9,
          },
          {
            period: '2024-04',
            period_start: '2024-04-01T00:00:00Z',
            new_accounts: 2,
            events: 3,
          },
        ],
        totals: {
          accounts: 6,
          events: 20,
          events_placed: 12,
          events_unplaced: 8,
        },
        lower_bound: true,
        unplaced: [
          {
            reason: 'repeat-events-not-timestamped',
            events: 8,
            detail:
              'an edge stores first_at and last_at only, so events between the first and the last of a repeated relationship have no recorded month',
          },
        ],
        coverage,
      },
    },
    note: 'History, not live state: creations are immutable and sponsorship figures count arrangements STARTED.',
  },
};

function renderPanel(relation: 'created' | 'sponsored' = 'sponsored') {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(
    <QueryClientProvider client={qc}>
      <AccountRelationHistory account={ACCOUNT} relation={relation} />
    </QueryClientProvider>,
  );
}

function chartPoints() {
  const raw = screen.getByTestId('line-chart').getAttribute('data-points');
  return JSON.parse(raw ?? '[]') as { time: number; value: number | null }[];
}

describe('AccountRelationHistory', () => {
  beforeEach(() => {
    apiGet.mockReset();
    apiGet.mockResolvedValue(history);
  });

  /**
   * THE FINDING THIS GUARDS. A quiet month emits no point, and a chart
   * fed the served points alone joins January to April with a straight
   * line — three months of invented activity drawn at the same
   * confidence as the measured ones. The gap has to reach the chart.
   */
  it('hands the chart a break for each quiet month, never an interpolated value', async () => {
    renderPanel();
    await screen.findByTestId('line-chart');

    const points = chartPoints();
    expect(points).toHaveLength(4);
    expect(points.map((p) => p.value)).toEqual([4, null, null, 2]);
  });

  it('says in the copy that the line breaks rather than reading zero', async () => {
    renderPanel();
    expect(
      await screen.findByText(/a month with no activity emits no point/i),
    ).toBeInTheDocument();
  });

  /**
   * The cumulative metric is the deliberate exception: a total that
   * gained nothing has not become unknown, so it crosses the gap — and
   * the copy has to say that is a fact rather than an interpolation.
   */
  it('carries the cumulative line across the gap and explains why that is not interpolation', async () => {
    renderPanel();
    await screen.findByTestId('line-chart');

    fireEvent.click(screen.getByRole('button', { name: 'Cumulative' }));

    expect(chartPoints().map((p) => p.value)).toEqual([4, 4, 4, 6]);
    expect(
      screen.getByText(/crosses a month with no activity unbroken/i),
    ).toBeInTheDocument();
  });

  /**
   * THE FINDING THIS GUARDS. `events` is a declared LOWER BOUND whenever
   * the series says so, and `unplaced[]` names exactly what was left
   * out. Drawing the line without either would be a smaller claim
   * wearing the full one's name.
   */
  it('labels the event series a lower bound and prints the unplaced register', async () => {
    renderPanel();
    const eventsOption = await screen.findByRole('button', {
      name: /Started \(lower bound\)/,
    });

    fireEvent.click(eventsOption);

    expect(screen.getByText('This line is a lower bound')).toBeInTheDocument();
    expect(
      screen.getByText('repeat-events-not-timestamped'),
    ).toBeInTheDocument();
    expect(screen.getByText(/— 8/)).toBeInTheDocument();
    expect(
      screen.getByText(/an edge stores first_at and last_at only/i),
    ).toBeInTheDocument();
  });

  it('shows how many events were placed and how many were not', async () => {
    renderPanel();
    expect(await screen.findByText('Placed in a month')).toBeInTheDocument();
    expect(screen.getByText('Not placeable')).toBeInTheDocument();
    expect(screen.getByText('12')).toBeInTheDocument();
  });

  it('carries the payload’s own history-not-live-state note', async () => {
    renderPanel();
    expect(
      await screen.findByText(/History, not live state/i),
    ).toBeInTheDocument();
  });

  it('states the coverage span the absent months are read against', async () => {
    renderPanel();
    const strip = await screen.findByText(
      /the span the rollup behind this arm/i,
    );
    expect(strip.textContent).toContain('32,747,295');
    expect(strip.textContent).toContain('64,428,050');
  });

  it('serves an empty series as an answer, not as a warming panel', async () => {
    renderPanel('created');
    expect(
      await screen.findByText(/no month in the covered span carries/i),
    ).toBeInTheDocument();
    expect(screen.queryByTestId('line-chart')).not.toBeInTheDocument();
  });

  /**
   * THE FINDING THIS GUARDS. graph/history postdates the deployed API on
   * some environments and 404s there (verified against production
   * 2026-09-15, where /graph answers 200 and /graph/history answers
   * 404). Calling that "warming" would tell a reader to wait for a
   * rollup cycle that has already run; the endpoint has to ship first.
   */
  it('does not call a missing endpoint a warming one', async () => {
    apiGet.mockRejectedValue(
      new Error(`404 Not Found on /v1/accounts/${ACCOUNT}/graph/history`),
    );
    renderPanel();

    expect(await screen.findByText(/does not serve/i)).toBeInTheDocument();
    expect(screen.queryByText(/warming/i)).not.toBeInTheDocument();
  });

  it('does call a warming endpoint a warming one', async () => {
    apiGet.mockRejectedValue(
      new Error(
        `503 Service Unavailable on /v1/accounts/${ACCOUNT}/graph/history`,
      ),
    );
    renderPanel();

    expect(await screen.findByText(/warming/i)).toBeInTheDocument();
  });
});
