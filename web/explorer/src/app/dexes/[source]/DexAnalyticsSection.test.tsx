import { describe, it, expect, vi } from 'vitest';
import { render, screen, waitFor } from '@testing-library/react';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';

import { DexAnalyticsSection } from './DexAnalyticsSection';

vi.mock('@/api/client', async () => {
  const actual = await vi.importActual<typeof import('@/api/client')>('@/api/client');
  return { ...actual, apiGet: vi.fn() };
});

// The chart canvases are next/dynamic + lightweight-charts; the semantics
// under test (KPIs, panel titles, donut legend, omitted series) are DOM.
vi.mock('next/dynamic', () => ({
  default: () => {
    const Stub = () => <div data-testid="chart-stub" />;
    return Stub;
  },
}));

import { apiGet } from '@/api/client';

function renderIt(source = 'soroswap') {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(
    <QueryClientProvider client={client}>
      <DexAnalyticsSection source={source} />
    </QueryClientProvider>,
  );
}

const BESPOKE = {
  category: 'dex',
  kpis: [{ label: 'USD volume (90d)', value: '$1.2M', unit: 'USD' }],
  series: [
    {
      name: 'USD volume',
      unit: 'USD',
      points: [{ date: '2026-07-28', value: '1000' }],
    },
    {
      name: 'Trades',
      unit: 'trades',
      points: [{ date: '2026-07-28', value: '42' }],
    },
    {
      name: 'Top pairs · XLM/USDC',
      unit: 'USD',
      points: [{ date: '2026-07-28', value: '900' }],
    },
  ],
  breakdowns: [
    {
      title: 'Volume by pair',
      unit: 'USD',
      rows: [{ label: 'XLM/USDC', value: '900', count: 40 }],
    },
  ],
};

describe('DexAnalyticsSection', () => {
  it('renders the bespoke suite (KPIs, series panels, top-pairs group, donut) minus the standalone USD-volume series', async () => {
    vi.mocked(apiGet).mockResolvedValue({ data: { bespoke: BESPOKE } });
    renderIt();
    await waitFor(() =>
      expect(screen.getByText('DEX analytics')).toBeInTheDocument(),
    );
    expect(screen.getByText('USD volume (90d)')).toBeInTheDocument(); // KPI card
    expect(screen.getByText('Trades')).toBeInTheDocument(); // standalone series panel
    expect(screen.getByText('Top pairs')).toBeInTheDocument(); // grouped multi-line panel
    expect(screen.getByText('Volume by pair')).toBeInTheDocument(); // donut hint carries the title
    // The standalone "USD volume" series is SourceVolumeHistory's panel —
    // it must not render a second time inside this section.
    expect(screen.queryByText('USD volume')).not.toBeInTheDocument();
  });

  it('renders nothing when the API serves no bespoke block — absence, not zeros', async () => {
    vi.mocked(apiGet).mockResolvedValue({ data: {} });
    const { container } = renderIt();
    await waitFor(() => expect(vi.mocked(apiGet)).toHaveBeenCalled());
    await waitFor(() => expect(container.firstChild).toBeNull());
  });
});
