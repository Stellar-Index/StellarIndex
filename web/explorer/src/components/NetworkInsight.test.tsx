import { describe, it, expect, vi } from 'vitest';
import { render, screen, waitFor } from '@testing-library/react';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';

vi.mock('@/api/client', async () => {
  const actual = await vi.importActual<typeof import('@/api/client')>('@/api/client');
  return { ...actual, apiGet: vi.fn() };
});

// Swap the real (canvas-based, lightweight-charts) LineChart for a plain
// DOM stand-in so the test can assert on exactly which points it was
// handed, without depending on canvas rendering under jsdom.
vi.mock('@/components/charts/LineChart', () => ({
  LineChart: ({ data }: { data: { time: number; value: number }[] }) => (
    <div data-testid="line-chart">{JSON.stringify(data.map((p) => p.value))}</div>
  ),
}));

import { apiGet } from '@/api/client';
import { ThroughputPanel } from './NetworkInsight';

// UXP-16: the API's own contract for GET /v1/network/throughput says a
// `partial: true` bucket (today, still accumulating) must be EXCLUDED
// from window totals and rendered distinctly — ThroughputPanel used to
// fold it straight into both `total` and the chart series.
describe('ThroughputPanel', () => {
  it('excludes the partial (still-accumulating) bucket from the total and the chart series', async () => {
    vi.mocked(apiGet).mockResolvedValue({
      data: {
        window_days: 3,
        buckets: [
          { day: '2026-07-20', ops: 1000 },
          { day: '2026-07-21', ops: 2000 },
          { day: '2026-07-22', ops: 999, partial: true },
        ],
      },
    });
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    render(
      <QueryClientProvider client={client}>
        <ThroughputPanel defaultMetric="ops" windowDays={3} />
      </QueryClientProvider>,
    );

    // Total: 1000 + 2000 = 3000, NOT 3999 (which would include the
    // partial day's 999).
    await waitFor(() => expect(screen.getByText(/3K total/)).toBeInTheDocument());
    expect(screen.queryByText(/4K total/)).not.toBeInTheDocument();

    // Chart series: only the two complete-day points, not the partial one.
    const chart = await screen.findByTestId('line-chart');
    expect(JSON.parse(chart.textContent ?? '[]')).toEqual([1000, 2000]);
  });
});
