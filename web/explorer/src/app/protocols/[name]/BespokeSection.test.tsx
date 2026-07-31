import { describe, it, expect, vi, beforeEach } from 'vitest';
import { render, screen, waitFor, fireEvent } from '@testing-library/react';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';

import { BespokeSection, splitSeriesGroups, toChartNumber } from './BespokeSection';
import type { Bespoke } from './BespokeSection';

vi.mock('@/api/client', async () => {
  const actual = await vi.importActual<typeof import('@/api/client')>('@/api/client');
  return { ...actual, apiGet: vi.fn() };
});

// Stub the canvas-rendered chart (same stub as BridgeShowcase.test.tsx):
// record single-series point counts OR the named multi-series composition.
vi.mock('@/components/charts/LineChart', () => ({
  LineChart: (props: {
    data: { time: number; value: number }[];
    series?: { label: string; data: { time: number; value: number }[]; color?: string }[];
    timeVisible?: boolean;
    ariaLabel?: string;
  }) => (
    <div
      data-testid="line-chart"
      role="img"
      aria-label={props.ariaLabel}
      data-points={props.data.length}
      data-series={(props.series ?? []).map((s) => `${s.label}:${s.data.length}`).join('|')}
      data-timevisible={String(props.timeVisible ?? false)}
    />
  ),
}));

import { apiGet, asExample } from '@/api/client';

// A DEX bespoke block in the server's 90d shape: window + since KPIs,
// standalone series, "Top pairs · <PAIR>" multi-series, the volume-by-pair
// breakdown, and the largest-trades table.
const dexBespoke: Bespoke = {
  category: 'dex',
  kpis: [
    { label: 'USD volume (90d)', value: '53218711.18', unit: 'USD' },
    { label: 'Trades (90d)', value: '973075' },
    { label: 'Unique traders (90d)', value: '82' },
    { label: 'Volume since 2026-03-18', value: '53218711.18', unit: 'USD' },
  ],
  series: [
    {
      name: 'USD volume',
      unit: 'USD',
      points: [
        { date: '2026-07-27', value: '1000.50' },
        { date: '2026-07-28', value: '2000.25' },
      ],
    },
    {
      name: 'Unique traders',
      unit: 'traders',
      points: [{ date: '2026-07-28', value: '42' }],
    },
    {
      name: 'Top pairs · XLM/USDC',
      unit: 'USD',
      points: [
        { date: '2026-07-27', value: '600.00' },
        { date: '2026-07-28', value: '900.00' },
      ],
    },
    {
      name: 'Top pairs · XLM/AQUA',
      unit: 'USD',
      points: [{ date: '2026-07-28', value: '400.00' }],
    },
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
      columns: ['Pair', 'Base amount', 'Quote amount', 'USD volume', 'Tx', 'Date'],
      rows: [
        [
          'CCCR…HGU2/USDC',
          '700707188872',
          '700000000000',
          '70000.00',
          '21d5cef1529d9100d63ed89ee5c86a06531b74055ba892512c2eda358ce8274e',
          '2026-06-15',
        ],
      ],
    },
  ],
  notes: ['USD figures are sums of the ingest-time trades.usd_volume valuation only.'],
};

function renderIt(name: string, bespoke: Bespoke) {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
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
    expect(standalone.map((s) => s.name)).toEqual(['USD volume', 'Unique traders']);
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
  it('parses plain and money/percent-decorated numeric strings', () => {
    expect(toChartNumber('1234.56')).toBe(1234.56);
    expect(toChartNumber('$1,234.56')).toBe(1234.56);
    expect(toChartNumber('12.5%')).toBe(12.5);
  });

  it('refuses non-numeric values instead of fabricating zeros', () => {
    expect(toChartNumber('')).toBeNull();
    expect(toChartNumber('—')).toBeNull();
    expect(toChartNumber('n/a')).toBeNull();
  });

  it('refuses compact-suffixed figures instead of mis-scaling them', () => {
    // "1.2M" stripped of its suffix used to plot as 1.2 — a 10^6 error.
    expect(toChartNumber('1.2M')).toBeNull();
    expect(toChartNumber('3.4K')).toBeNull();
  });
});

describe('BespokeSection (partial trailing daily bucket)', () => {
  it("drops today's accumulating UTC bucket from daily series charts", async () => {
    const d = (offset: number) =>
      new Date(Date.now() - offset * 86_400_000).toISOString().slice(0, 10);
    renderIt('soroswap', {
      category: 'dex',
      series: [
        {
          name: 'USD volume',
          unit: 'USD',
          points: [
            { date: d(2), value: '1' },
            { date: d(1), value: '2' },
            { date: d(0), value: '3' }, // today — still accumulating
          ],
        },
      ],
    });
    await waitFor(() => expect(screen.getAllByTestId('line-chart').length).toBeGreaterThan(0));
    // Only the two COMPLETE days plot; the partial day is not a cliff.
    expect(screen.getAllByTestId('line-chart')[0].getAttribute('data-points')).toBe('2');
  });
});

describe('BespokeSection (non-bridge window reactivity)', () => {
  it('renders the DEX suite with section-level pills and no duplicate fetch at the default window', async () => {
    renderIt('soroswap', dexBespoke);

    // Pills live at section level for EVERY category.
    for (const label of ['24h', '7d', '30d', '90d']) {
      expect(screen.getByRole('button', { name: label })).toBeInTheDocument();
    }
    expect(screen.getByRole('button', { name: '90d' })).toHaveAttribute('aria-pressed', 'true');

    // KPIs, standalone series panels, the grouped multi-line panel, the
    // donut and the table all render from the initial block.
    expect(screen.getByText('USD volume (90d)')).toBeInTheDocument();
    expect(screen.getByText('Volume since 2026-03-18')).toBeInTheDocument();
    await waitFor(() => expect(screen.getAllByTestId('line-chart').length).toBeGreaterThan(0));
    const charts = screen.getAllByTestId('line-chart');
    // Grouped top-pairs series render as ONE multi-series chart with bare
    // pair labels — not one panel per pair.
    expect(
      charts.some((c) => c.getAttribute('data-series') === 'XLM/USDC:2|XLM/AQUA:1'),
    ).toBe(true);
    expect(screen.getByText('Top pairs')).toBeInTheDocument();
    expect(screen.getByText('Volume by pair')).toBeInTheDocument();
    expect(screen.getByText('Largest trades')).toBeInTheDocument();

    // Largest-trades tx hash links to the canonical transaction route.
    const txLink = screen
      .getAllByRole('link')
      .find((a) =>
        a
          .getAttribute('href')
          ?.startsWith(
            '/transactions/21d5cef1529d9100d63ed89ee5c86a06531b74055ba892512c2eda358ce8274e',
          ),
      );
    expect(txLink).toBeDefined();

    // The 90d default reuses the page's initial data — no refetch.
    expect(vi.mocked(apiGet)).not.toHaveBeenCalled();
  });

  it('refetches the WHOLE block (KPIs included) on pill click and switches to the hourly axis', async () => {
    vi.mocked(apiGet).mockResolvedValue({
      data: {
        bespoke: {
          category: 'dex',
          kpis: [{ label: 'USD volume (1d)', value: '1234.56', unit: 'USD' }],
          series: [
            {
              name: 'USD volume',
              unit: 'USD',
              points: [
                { date: '2026-07-29T13:00', value: '10' },
                { date: '2026-07-29T14:00', value: '20' },
                { date: '2026-07-29T15:00', value: '30' },
              ],
            },
          ],
        },
      },
    });
    renderIt('soroswap', dexBespoke);

    fireEvent.click(screen.getByRole('button', { name: '24h' }));
    await waitFor(() =>
      expect(vi.mocked(apiGet)).toHaveBeenCalledWith('/v1/protocols/soroswap?days=1'),
    );
    // The refetched window's KPI replaces the 90d one.
    await waitFor(() => expect(screen.getByText('USD volume (1d)')).toBeInTheDocument());
    expect(screen.queryByText('USD volume (90d)')).not.toBeInTheDocument();
    // The hourly series renders with the time-of-day axis.
    await waitFor(() =>
      expect(
        screen
          .getAllByTestId('line-chart')
          .some((c) => c.getAttribute('data-points') === '3'),
      ).toBe(true),
    );
    const hourly = screen
      .getAllByTestId('line-chart')
      .find((c) => c.getAttribute('data-points') === '3');
    expect(hourly!.getAttribute('data-timevisible')).toBe('true');
    expect(screen.getByRole('button', { name: '24h' })).toHaveAttribute('aria-pressed', 'true');
  });

  it('shows an honest empty state when the refetched window has no bespoke block', async () => {
    vi.mocked(apiGet).mockResolvedValue({ data: {} });
    renderIt('soroswap', dexBespoke);

    fireEvent.click(screen.getByRole('button', { name: '7d' }));
    await waitFor(() =>
      expect(vi.mocked(apiGet)).toHaveBeenCalledWith('/v1/protocols/soroswap?days=7'),
    );
    await waitFor(() =>
      expect(screen.getByText('No analytics in this window.')).toBeInTheDocument(),
    );
    // The pills stay reachable so the user can navigate back.
    expect(screen.getByRole('button', { name: '90d' })).toBeInTheDocument();
  });

  it('shows an honest error state when the window refetch fails', async () => {
    vi.mocked(apiGet).mockRejectedValue(new Error('boom'));
    renderIt('soroswap', dexBespoke);

    fireEvent.click(screen.getByRole('button', { name: '30d' }));
    await waitFor(() =>
      expect(
        screen.getByText(/Couldn't load this window/),
      ).toBeInTheDocument(),
    );
  });

  it('returns to the initial block without a new fetch when switching back to 90d', async () => {
    vi.mocked(apiGet).mockResolvedValue({
      data: {
        bespoke: { category: 'dex', kpis: [{ label: 'Trades (7d)', value: '9' }] },
      },
    });
    renderIt('soroswap', dexBespoke);

    fireEvent.click(screen.getByRole('button', { name: '7d' }));
    await waitFor(() => expect(screen.getByText('Trades (7d)')).toBeInTheDocument());
    const calls = vi.mocked(apiGet).mock.calls.length;

    fireEvent.click(screen.getByRole('button', { name: '90d' }));
    await waitFor(() => expect(screen.getByText('USD volume (90d)')).toBeInTheDocument());
    expect(vi.mocked(apiGet).mock.calls.length).toBe(calls);
  });
});
