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

// Publish the points handed to the canvas chart back to the DOM.
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

// February and March are QUIET: the endpoint emits no point for them.
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
          ['2024-01', 4, 9],
          ['2024-04', 2, 3],
        ].map(([period, new_accounts, events]) => ({
          period,
          period_start: `${period}-01T00:00:00Z`,
          new_accounts,
          events,
        })),
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

  // A chart fed the served points alone joins January to April with a
  // straight line: invented activity drawn as if measured.
  it('hands the chart a break for each quiet month, never an interpolated value', async () => {
    renderPanel();
    await screen.findByTestId('line-chart');

    const points = chartPoints();
    expect(points).toHaveLength(4);
    expect(points.map((p) => p.value)).toEqual([4, null, null, 2]);
  });

  it('carries the cumulative line across the gap and explains why that is not interpolation', async () => {
    renderPanel();
    await screen.findByTestId('line-chart');

    fireEvent.click(screen.getByRole('button', { name: 'Cumulative' }));

    expect(chartPoints().map((p) => p.value)).toEqual([4, 4, 4, 6]);
    expect(
      screen.getByText(/crosses a month with no activity unbroken/i),
    ).toBeInTheDocument();
  });

  // `events` is a declared lower bound; unplaced[] names what was left out.
  it('labels the event series a lower bound and prints the unplaced register', async () => {
    renderPanel();
    fireEvent.click(
      await screen.findByRole('button', { name: /Started \(lower bound\)/ }),
    );

    expect(screen.getByText('This line is a lower bound')).toBeInTheDocument();
    expect(
      screen.getByText('repeat-events-not-timestamped'),
    ).toBeInTheDocument();
    expect(screen.getByText(/— 8/)).toBeInTheDocument();
    expect(
      screen.getByText(/an edge stores first_at and last_at only/i),
    ).toBeInTheDocument();
  });

  it('states the break copy, the placed/unplaced split, the payload note and the coverage span', async () => {
    renderPanel();
    expect(
      await screen.findByText(/a month with no activity emits no point/i),
    ).toBeInTheDocument();
    expect(screen.getByText('Placed in a month')).toBeInTheDocument();
    expect(screen.getByText('Not placeable')).toBeInTheDocument();
    expect(screen.getByText('12')).toBeInTheDocument();
    expect(screen.getByText(/History, not live state/i)).toBeInTheDocument();
    const strip = screen.getByText(/the span the rollup behind this arm/i);
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

  // graph/history 404s on deployments older than the endpoint; calling that
  // "warming" would tell a reader to wait for a rollup that already ran.
  it.each([
    ['404 Not Found', /does not serve/i, false],
    ['503 Service Unavailable', /warming/i, true],
  ])('classifies a %s failure correctly', async (status, text, warming) => {
    apiGet.mockRejectedValue(
      new Error(`${status} on /v1/accounts/${ACCOUNT}/graph/history`),
    );
    renderPanel();

    expect(await screen.findByText(text)).toBeInTheDocument();
    if (!warming) {
      expect(screen.queryByText(/warming/i)).not.toBeInTheDocument();
    }
  });
});
