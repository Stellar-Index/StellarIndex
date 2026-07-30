import { describe, it, expect, vi } from 'vitest';
import { render, screen, waitFor, fireEvent } from '@testing-library/react';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';

import {
  BridgeShowcase,
  flowLines,
  perChainLines,
  donutSlices,
  chainColor,
} from './BridgeShowcase';
import type { Bespoke, BespokeBreakdown } from './BespokeSection';

vi.mock('@/api/client', async () => {
  const actual = await vi.importActual<typeof import('@/api/client')>('@/api/client');
  return { ...actual, apiGet: vi.fn() };
});

// Stub the canvas-rendered chart: record single-series point counts OR the
// named multi-series composition (labels, point counts, colors) so the tests
// assert semantics without lightweight-charts needing a real canvas.
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
      data-colors={(props.series ?? []).map((s) => s.color ?? '').join('|')}
      data-timevisible={String(props.timeVisible ?? false)}
    />
  ),
}));

import { apiGet, asExample } from '@/api/client';

const cctpBespoke: Bespoke = {
  category: 'bridge',
  series: [
    {
      name: 'Inbound (USDC)',
      unit: 'USDC',
      points: [
        { date: '2026-07-27', value: '1000.50' },
        { date: '2026-07-28', value: '2000.25' },
      ],
    },
    {
      name: 'Outbound (USDC)',
      unit: 'USDC',
      points: [
        { date: '2026-07-27', value: '500.10' },
        { date: '2026-07-28', value: '750.00' },
      ],
    },
    {
      name: 'Inbound · Base',
      unit: 'USDC',
      points: [
        { date: '2026-07-27', value: '600.00' },
        { date: '2026-07-28', value: '900.00' },
      ],
    },
    {
      name: 'Inbound · Solana',
      unit: 'USDC',
      points: [{ date: '2026-07-28', value: '400.00' }],
    },
    {
      name: 'Outbound · Ethereum',
      unit: 'USDC',
      points: [{ date: '2026-07-28', value: '750.00' }],
    },
    {
      name: 'Cumulative net inflow (all-time)',
      unit: 'USDC',
      points: [
        { date: '2026-01-01', value: '100.00' },
        { date: '2026-07-28', value: '8003390.36' },
      ],
    },
  ],
  breakdowns: [
    {
      title: 'Inflows by source chain',
      unit: 'USDC',
      rows: [
        { label: 'Base', value: '6432257.50', count: 6063 },
        { label: 'Ethereum', value: '2742082.73', count: 146 },
        { label: 'Solana', value: '2001146.57', count: 1893 },
        { label: 'Unverified (0x3600…0000)', value: '0.01', count: 1 },
      ],
    },
    {
      title: 'Outflows by destination chain',
      unit: 'USDC',
      rows: [
        { label: 'Solana', value: '1802090.41', count: 2395 },
        { label: 'Ethereum', value: '1107891.94', count: 96 },
      ],
    },
  ],
  tables: [
    {
      title: 'Largest transfers',
      columns: ['Direction', 'Chain', 'Amount (USDC)', 'Tx', 'Date'],
      rows: [
        [
          'Inbound',
          'Ethereum',
          '901413.243537',
          '71689b2f79215976f6099f0c705b3ffb4ff8f2f40d1ee2b43636c699e71fbffe',
          '2026-06-11',
        ],
      ],
    },
  ],
};

const rozoBespoke: Bespoke = {
  category: 'bridge',
  series: [
    {
      name: 'Settled volume (USDC)',
      unit: 'USDC',
      points: [{ date: '2026-07-28', value: '42.5' }],
    },
  ],
};

function renderIt(name: string, initial: Bespoke) {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(
    <QueryClientProvider client={client}>
      <BridgeShowcase name={name} initial={initial} source={asExample(`/v1/protocols/${name}`)} />
    </QueryClientProvider>,
  );
}

describe('flowLines', () => {
  it('pairs the exact-name inbound + outbound totals, ignoring per-chain and cumulative series', () => {
    const lines = flowLines(cctpBespoke.series!);
    expect(lines.map((l) => l.label)).toEqual(['Inbound (USDC)', 'Outbound (USDC)']);
    expect(lines[0].tone).not.toBe(lines[1].tone);
    expect(lines[0].data).toHaveLength(2);
  });

  it('renders a lone series (rozo settled volume) as a single line', () => {
    const lines = flowLines(rozoBespoke.series!);
    expect(lines).toHaveLength(1);
    expect(lines[0].label).toBe('Settled volume (USDC)');
  });

  it('never falls back to an auxiliary series when the totals are absent', () => {
    const lines = flowLines([
      { name: 'Inbound · Base', points: [{ date: '2026-07-28', value: '1' }] },
      {
        name: 'Cumulative net inflow (all-time)',
        points: [{ date: '2026-07-28', value: '2' }],
      },
    ]);
    expect(lines).toHaveLength(0);
  });

  it('parses hourly point timestamps (the 24h window shape)', () => {
    const lines = flowLines([
      {
        name: 'Inbound (USDC)',
        points: [
          { date: '2026-07-29T13:00', value: '1' },
          { date: '2026-07-29T14:00', value: '2' },
        ],
      },
      { name: 'Outbound (USDC)', points: [{ date: '2026-07-29T13:00', value: '3' }] },
    ]);
    // Consecutive hourly buckets are exactly 3600s apart.
    expect(lines[0].data[1].time - lines[0].data[0].time).toBe(3600);
  });
});

describe('perChainLines', () => {
  it('extracts the prefixed series as chain-labelled lines with stable entity colors', () => {
    const lines = perChainLines(cctpBespoke.series!, 'Inbound · ');
    expect(lines.map((l) => l.label)).toEqual(['Base', 'Solana']);
    // Color follows the entity: the same chain gets the same hue wherever
    // it appears, and two chains never share one.
    expect(lines[0].color).toBe(chainColor('Base'));
    expect(lines[1].color).toBe(chainColor('Solana'));
    expect(lines[0].color).not.toBe(lines[1].color);
  });

  it('keeps directions separate', () => {
    const lines = perChainLines(cctpBespoke.series!, 'Outbound · ');
    expect(lines.map((l) => l.label)).toEqual(['Ethereum']);
  });
});

describe('donutSlices', () => {
  it('keeps honest unknown labels as their own slice', () => {
    const slices = donutSlices(cctpBespoke.breakdowns![0]);
    expect(slices.map((s) => s.label)).toEqual([
      'Base',
      'Ethereum',
      'Solana',
      'Unverified (0x3600…0000)',
    ]);
  });

  it('folds slices beyond the top 6 into an Others bucket', () => {
    const b: BespokeBreakdown = {
      title: 'Inflows by source chain',
      unit: 'USDC',
      rows: Array.from({ length: 9 }, (_, i) => ({
        label: `Chain${i}`,
        value: String(900 - i * 100),
        count: 1,
      })),
    };
    const slices = donutSlices(b);
    expect(slices).toHaveLength(7);
    expect(slices[6].label).toBe('Others (3)');
    // 300 + 200 + 100 folded together.
    expect(slices[6].value).toBe(600);
  });
});

describe('BridgeShowcase', () => {
  it('renders the full cctp suite: cumulative headline, dual flow chart, donuts, per-chain charts and the transfers table', async () => {
    renderIt('cctp', cctpBespoke);

    // Window pills.
    for (const label of ['24h', '7d', '30d', '90d']) {
      expect(screen.getByRole('button', { name: label })).toBeInTheDocument();
    }
    expect(screen.getByRole('button', { name: '90d' })).toHaveAttribute('aria-pressed', 'true');

    await waitFor(() => expect(screen.getAllByTestId('line-chart').length).toBeGreaterThan(0));
    const charts = screen.getAllByTestId('line-chart');

    // The all-time cumulative headline renders as a single-series chart.
    expect(screen.getByText('Cumulative net inflow')).toBeInTheDocument();
    const cumulativeChart = charts.find((c) => c.getAttribute('data-points') === '2');
    expect(cumulativeChart).toBeDefined();
    expect(cumulativeChart!.getAttribute('aria-label')).toMatch(/Cumulative net USDC inflow/);

    // The combined flow chart carries BOTH totals lines (per-chain and
    // cumulative series stay off it).
    expect(
      charts.some(
        (c) => c.getAttribute('data-series') === 'Inbound (USDC):2|Outbound (USDC):2',
      ),
    ).toBe(true);

    // Per-chain multi-line charts render with bare chain labels.
    expect(charts.some((c) => c.getAttribute('data-series') === 'Base:2|Solana:1')).toBe(true);
    expect(charts.some((c) => c.getAttribute('data-series') === 'Ethereum:1')).toBe(true);
    expect(screen.getByText('Inflows by source chain')).toBeInTheDocument();
    expect(screen.getByText('Outflows by destination chain')).toBeInTheDocument();

    // Donuts: friendly headings + shares + honest unverified label.
    expect(screen.getByText('Where funds come from')).toBeInTheDocument();
    expect(screen.getByText('Where funds go')).toBeInTheDocument();
    // Base share of 6432257.50 / 11175486.81 ≈ 57.6%.
    expect(screen.getByText('57.6%')).toBeInTheDocument();
    expect(screen.getByText('Unverified (0x3600…0000)')).toBeInTheDocument();

    // Largest transfers table renders with the tx hash linked to the
    // canonical transaction route.
    expect(screen.getByText('Largest transfers')).toBeInTheDocument();
    const txLink = screen
      .getAllByRole('link')
      .find((a) =>
        a
          .getAttribute('href')
          ?.startsWith(
            '/transactions/71689b2f79215976f6099f0c705b3ffb4ff8f2f40d1ee2b43636c699e71fbffe',
          ),
      );
    expect(txLink).toBeDefined();

    // The 90d default reuses the page's initial data — no duplicate fetch.
    expect(vi.mocked(apiGet)).not.toHaveBeenCalled();
  });

  it('refetches the whole block on pill click, switches to hourly axis, and keeps the cumulative headline from the initial fetch', async () => {
    vi.mocked(apiGet).mockResolvedValue({
      data: {
        bespoke: {
          category: 'bridge',
          series: [
            {
              name: 'Inbound (USDC)',
              unit: 'USDC',
              points: [
                { date: '2026-07-29T13:00', value: '10' },
                { date: '2026-07-29T14:00', value: '20' },
                { date: '2026-07-29T15:00', value: '30' },
              ],
            },
            {
              name: 'Outbound (USDC)',
              unit: 'USDC',
              points: [{ date: '2026-07-29T13:00', value: '5' }],
            },
            {
              name: 'Cumulative net inflow (all-time)',
              unit: 'USDC',
              points: [
                { date: '2026-01-01', value: '100.00' },
                { date: '2026-07-28', value: '8003390.36' },
                { date: '2026-07-29', value: '8003400.00' },
              ],
            },
          ],
        },
      },
    });
    renderIt('cctp', cctpBespoke);

    fireEvent.click(screen.getByRole('button', { name: '24h' }));
    await waitFor(() =>
      expect(vi.mocked(apiGet)).toHaveBeenCalledWith('/v1/protocols/cctp?days=1'),
    );
    await waitFor(() =>
      expect(
        screen
          .getAllByTestId('line-chart')
          .some(
            (c) => c.getAttribute('data-series') === 'Inbound (USDC):3|Outbound (USDC):1',
          ),
      ).toBe(true),
    );
    const flow = screen
      .getAllByTestId('line-chart')
      .find((c) => c.getAttribute('data-series') === 'Inbound (USDC):3|Outbound (USDC):1');
    expect(flow!.getAttribute('data-timevisible')).toBe('true');
    expect(screen.getByRole('button', { name: '24h' })).toHaveAttribute('aria-pressed', 'true');

    // Cumulative headline still shows the INITIAL all-time series (2 points),
    // not the refetched window's copy.
    expect(
      screen
        .getAllByTestId('line-chart')
        .some((c) => c.getAttribute('data-points') === '2'),
    ).toBe(true);
  });

  it('shows an honest empty state when the window has no transfers', async () => {
    vi.mocked(apiGet).mockResolvedValue({
      data: { bespoke: { category: 'bridge', series: [] } },
    });
    renderIt('rozo', rozoBespoke);

    fireEvent.click(screen.getByRole('button', { name: '7d' }));
    await waitFor(() =>
      expect(vi.mocked(apiGet)).toHaveBeenCalledWith('/v1/protocols/rozo?days=7'),
    );
    await waitFor(() =>
      expect(screen.getByText('No transfers in this window.')).toBeInTheDocument(),
    );
    expect(screen.queryByTestId('line-chart')).not.toBeInTheDocument();
  });

  it('renders the single settled-volume line for rozo without donuts or extra panels', async () => {
    renderIt('rozo', rozoBespoke);
    await waitFor(() => expect(screen.getAllByTestId('line-chart')).toHaveLength(1));
    expect(screen.getAllByTestId('line-chart')[0]).toHaveAttribute(
      'data-series',
      'Settled volume (USDC):1',
    );
    expect(screen.queryByText('Where funds come from')).not.toBeInTheDocument();
    expect(screen.queryByText('Cumulative net inflow')).not.toBeInTheDocument();
  });
});
