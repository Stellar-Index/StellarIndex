import { describe, it, expect, vi, beforeEach } from 'vitest';
import { render, screen, waitFor, fireEvent } from '@testing-library/react';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';

import {
  BespokeSection,
  splitSeriesGroups,
  toChartNumber,
} from './BespokeSection';
import type { Bespoke } from './BespokeSection';

vi.mock('@/api/client', async () => {
  const actual =
    await vi.importActual<typeof import('@/api/client')>('@/api/client');
  return { ...actual, apiGet: vi.fn() };
});

// Canvas chart stub: records point counts and the multi-series composition.
vi.mock('@/components/charts/LineChart', () => ({
  LineChart: (props: {
    data: unknown[];
    series?: { label: string; data: unknown[] }[];
    timeVisible?: boolean;
    ariaLabel?: string;
  }) => (
    <div
      data-testid="line-chart"
      role="img"
      aria-label={props.ariaLabel}
      data-points={props.data.length}
      data-series={(props.series ?? [])
        .map((s) => `${s.label}:${s.data.length}`)
        .join('|')}
      data-timevisible={String(props.timeVisible ?? false)}
    />
  ),
}));

import { apiGet, asExample } from '@/api/client';

// Series fixture: points are [date, value] pairs.
const series = (name: string, unit: string, pts: [string, string][]) => ({
  name,
  unit,
  points: pts.map(([date, value]) => ({ date, value })),
});

// A DEX bespoke block in the server's 90d shape: window + since KPIs,
// standalone series, "Top pairs · <PAIR>" multi-series, the volume-by-pair
// breakdown, and the largest-trades table.
const TX = '21d5cef1529d9100d63ed89ee5c86a06531b74055ba892512c2eda358ce8274e';
const dexBespoke: Bespoke = {
  category: 'dex',
  kpis: [
    { label: 'USD volume (90d)', value: '53218711.18', unit: 'USD' },
    { label: 'Trades (90d)', value: '973075' },
    { label: 'Unique traders (90d)', value: '82' },
    { label: 'Volume since 2026-03-18', value: '53218711.18', unit: 'USD' },
  ],
  series: [
    series('USD volume', 'USD', [
      ['2026-07-27', '1000.50'],
      ['2026-07-28', '2000.25'],
    ]),
    series('Unique traders', 'traders', [['2026-07-28', '42']]),
    series('Top pairs · XLM/USDC', 'USD', [
      ['2026-07-27', '600.00'],
      ['2026-07-28', '900.00'],
    ]),
    series('Top pairs · XLM/AQUA', 'USD', [['2026-07-28', '400.00']]),
  ],
  breakdowns: [
    {
      title: 'Volume by pair',
      unit: 'USD',
      rows: [
        { label: 'XLM/USDC', value: '45619756.91', count: 220556 },
        { label: 'USDC/XLM', value: '2151094.26', count: 221634 },
        { label: 'Others', value: '1702829.73', count: 446444 },
      ],
    },
  ],
  tables: [
    {
      title: 'Largest trades',
      columns: [
        'Pair',
        'Base amount',
        'Quote amount',
        'USD volume',
        'Tx',
        'Date',
      ],
      rows: [
        [
          'CCCR…HGU2/USDC',
          '700707188872',
          '700000000000',
          '70000.00',
          TX,
          '2026-06-15',
        ],
      ],
    },
  ],
  notes: [
    'USD figures are sums of the ingest-time trades.usd_volume valuation only.',
  ],
};

function renderIt(name: string, bespoke: Bespoke) {
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  });
  return render(
    <QueryClientProvider client={client}>
      <BespokeSection
        bespoke={bespoke}
        name={name}
        source={asExample(`/v1/protocols/${name}`)}
      />
    </QueryClientProvider>,
  );
}

beforeEach(() => {
  vi.mocked(apiGet).mockReset();
});

describe('splitSeriesGroups', () => {
  it('folds "<Group> · <entity>" series into one group and keeps the rest standalone', () => {
    const { standalone, groups } = splitSeriesGroups(dexBespoke.series!);
    expect(standalone.map((s) => s.name)).toEqual([
      'USD volume',
      'Unique traders',
    ]);
    expect(groups).toHaveLength(1);
    expect(groups[0].title).toBe('Top pairs');
    expect(groups[0].series.map((s) => s.name)).toEqual([
      'Top pairs · XLM/USDC',
      'Top pairs · XLM/AQUA',
    ]);
  });

  it('keeps a name that starts with the separator standalone', () => {
    const { standalone, groups } = splitSeriesGroups([
      { name: ' · odd', points: [] },
    ]);
    expect(standalone).toHaveLength(1);
    expect(groups).toHaveLength(0);
  });
});

describe('toChartNumber', () => {
  it.each([
    ['1234.56', 1234.56],
    ['$1,234.56', 1234.56],
    ['12.5%', 12.5],
    // Non-numeric: refused, never fabricated zeros.
    ['', null],
    ['—', null],
    ['n/a', null],
    // "1.2M" stripped of its suffix would plot as 1.2, a 10^6 error.
    ['1.2M', null],
    ['3.4K', null],
  ])('%j -> %j', (input, want) => {
    expect(toChartNumber(input)).toBe(want);
  });
});

const charts = () => screen.getAllByTestId('line-chart');
const pill = (name: string) => screen.getByRole('button', { name });
const chartWith = (points: string) =>
  charts().find((c) => c.getAttribute('data-points') === points);

describe('BespokeSection (partial trailing daily bucket)', () => {
  it("drops today's accumulating UTC bucket from daily series charts", async () => {
    const d = (offset: number) =>
      new Date(Date.now() - offset * 86_400_000).toISOString().slice(0, 10);
    renderIt('soroswap', {
      category: 'dex',
      // d(0) is today, still accumulating.
      series: [
        series('USD volume', 'USD', [
          [d(2), '1'],
          [d(1), '2'],
          [d(0), '3'],
        ]),
      ],
    });
    await waitFor(() => expect(charts().length).toBeGreaterThan(0));
    // Only the two COMPLETE days plot; the partial day is not a cliff.
    expect(charts()[0].getAttribute('data-points')).toBe('2');
  });
});

describe('BespokeSection (non-bridge window reactivity)', () => {
  it('renders the DEX suite with section-level pills and no duplicate fetch at the default window', async () => {
    renderIt('soroswap', dexBespoke);

    for (const label of ['24h', '7d', '30d', '90d']) {
      expect(pill(label)).toBeInTheDocument();
    }
    expect(pill('90d')).toHaveAttribute('aria-pressed', 'true');

    expect(screen.getByText('USD volume (90d)')).toBeInTheDocument();
    expect(screen.getByText('Volume since 2026-03-18')).toBeInTheDocument();
    await waitFor(() => expect(charts().length).toBeGreaterThan(0));
    // Grouped top-pairs series render as ONE multi-series chart with bare
    // pair labels, not one panel per pair.
    expect(
      charts().some(
        (c) => c.getAttribute('data-series') === 'XLM/USDC:2|XLM/AQUA:1',
      ),
    ).toBe(true);
    expect(screen.getByText('Top pairs')).toBeInTheDocument();
    expect(screen.getByText('Volume by pair')).toBeInTheDocument();
    expect(screen.getByText('Largest trades')).toBeInTheDocument();

    // Largest-trades tx hash links to the canonical transaction route.
    const txLink = screen
      .getAllByRole('link')
      .find((a) => a.getAttribute('href')?.startsWith(`/transactions/${TX}`));
    expect(txLink).toBeDefined();

    // The 90d default reuses the page's initial data, no refetch.
    expect(vi.mocked(apiGet)).not.toHaveBeenCalled();
  });

  it('refetches the WHOLE block (KPIs included) on pill click and switches to the hourly axis', async () => {
    vi.mocked(apiGet).mockResolvedValue({
      data: {
        bespoke: {
          category: 'dex',
          kpis: [{ label: 'USD volume (1d)', value: '1234.56', unit: 'USD' }],
          series: [
            series('USD volume', 'USD', [
              ['2026-07-29T13:00', '10'],
              ['2026-07-29T14:00', '20'],
              ['2026-07-29T15:00', '30'],
            ]),
          ],
        },
      },
    });
    renderIt('soroswap', dexBespoke);

    fireEvent.click(pill('24h'));
    await waitFor(() =>
      expect(vi.mocked(apiGet)).toHaveBeenCalledWith(
        '/v1/protocols/soroswap?days=1',
      ),
    );
    await waitFor(() =>
      expect(screen.getByText('USD volume (1d)')).toBeInTheDocument(),
    );
    expect(screen.queryByText('USD volume (90d)')).not.toBeInTheDocument();
    await waitFor(() => expect(chartWith('3')).toBeDefined());
    expect(chartWith('3')!.getAttribute('data-timevisible')).toBe('true');
    expect(pill('24h')).toHaveAttribute('aria-pressed', 'true');
  });

  it.each([
    ['7d', 7, { data: {} }, 'No analytics in this window.'],
    ['30d', 30, new Error('boom'), /Couldn't load this window/],
  ] as const)(
    'shows an honest state on the %s refetch: %s',
    async (label, days, reply, text) => {
      if (reply instanceof Error) {
        vi.mocked(apiGet).mockRejectedValue(reply);
      } else {
        vi.mocked(apiGet).mockResolvedValue(reply);
      }
      renderIt('soroswap', dexBespoke);

      fireEvent.click(pill(label));
      await waitFor(() =>
        expect(vi.mocked(apiGet)).toHaveBeenCalledWith(
          `/v1/protocols/soroswap?days=${days}`,
        ),
      );
      await waitFor(() => expect(screen.getByText(text)).toBeInTheDocument());
      // The pills stay reachable so the user can navigate back.
      expect(pill('90d')).toBeInTheDocument();
    },
  );

  it('returns to the initial block without a new fetch when switching back to 90d', async () => {
    vi.mocked(apiGet).mockResolvedValue({
      data: {
        bespoke: {
          category: 'dex',
          kpis: [{ label: 'Trades (7d)', value: '9' }],
        },
      },
    });
    renderIt('soroswap', dexBespoke);

    fireEvent.click(pill('7d'));
    await waitFor(() =>
      expect(screen.getByText('Trades (7d)')).toBeInTheDocument(),
    );
    const calls = vi.mocked(apiGet).mock.calls.length;

    fireEvent.click(pill('90d'));
    await waitFor(() =>
      expect(screen.getByText('USD volume (90d)')).toBeInTheDocument(),
    );
    expect(vi.mocked(apiGet).mock.calls.length).toBe(calls);
  });
});
