import { describe, it, expect, vi } from 'vitest';
import { render, screen, waitFor, fireEvent } from '@testing-library/react';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';

import { BridgeFlowsChart, flowLines } from './BridgeFlowsChart';
import type { Bespoke } from './BespokeSection';

vi.mock('@/api/client', async () => {
  const actual = await vi.importActual<typeof import('@/api/client')>('@/api/client');
  return { ...actual, apiGet: vi.fn() };
});

// Stub the canvas-rendered chart: record the named series + axis mode so the
// tests assert semantics (which lines, how many points) without lightweight-
// charts needing a real canvas.
vi.mock('@/components/charts/LineChart', () => ({
  LineChart: (props: {
    series?: { label: string; data: { time: number; value: number }[] }[];
    timeVisible?: boolean;
  }) => (
    <div
      data-testid="line-chart"
      data-series={(props.series ?? []).map((s) => `${s.label}:${s.data.length}`).join('|')}
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
      <BridgeFlowsChart name={name} initial={initial} source={asExample(`/v1/protocols/${name}`)} />
    </QueryClientProvider>,
  );
}

describe('flowLines', () => {
  it('pairs inbound + outbound into two lines with distinct tones', () => {
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

describe('BridgeFlowsChart', () => {
  it('renders both directions on one chart with legend + pills, without refetching the default window', async () => {
    renderIt('cctp', cctpBespoke);

    // Window pills.
    for (const label of ['24h', '7d', '30d', '90d']) {
      expect(screen.getByRole('button', { name: label })).toBeInTheDocument();
    }
    expect(screen.getByRole('button', { name: '90d' })).toHaveAttribute('aria-pressed', 'true');

    // One combined chart carrying BOTH lines.
    await waitFor(() => expect(screen.getByTestId('line-chart')).toBeInTheDocument());
    expect(screen.getByTestId('line-chart')).toHaveAttribute(
      'data-series',
      'Inbound (USDC):2|Outbound (USDC):2',
    );

    // Legend names both directions.
    expect(screen.getByText('Inbound (USDC)')).toBeInTheDocument();
    expect(screen.getByText('Outbound (USDC)')).toBeInTheDocument();

    // The 90d default reuses the page's initial data — no duplicate fetch.
    expect(vi.mocked(apiGet)).not.toHaveBeenCalled();
  });

  it('refetches the window on pill click and switches the 24h window to hourly axis', async () => {
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
      expect(screen.getByTestId('line-chart')).toHaveAttribute(
        'data-series',
        'Inbound (USDC):3|Outbound (USDC):1',
      ),
    );
    expect(screen.getByTestId('line-chart')).toHaveAttribute('data-timevisible', 'true');
    expect(screen.getByRole('button', { name: '24h' })).toHaveAttribute('aria-pressed', 'true');
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

  it('renders the single settled-volume line for rozo', async () => {
    renderIt('rozo', rozoBespoke);
    await waitFor(() => expect(screen.getByTestId('line-chart')).toBeInTheDocument());
    expect(screen.getByTestId('line-chart')).toHaveAttribute(
      'data-series',
      'Settled volume (USDC):1',
    );
  });
});
