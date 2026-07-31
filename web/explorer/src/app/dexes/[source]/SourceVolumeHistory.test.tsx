import { describe, it, expect, vi } from 'vitest';
import { render, screen, waitFor } from '@testing-library/react';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';

import { SourceVolumeHistory, isUsdVolumeSeries } from './SourceVolumeHistory';

vi.mock('@/api/client', async () => {
  const actual = await vi.importActual<typeof import('@/api/client')>('@/api/client');
  return { ...actual, apiGet: vi.fn() };
});

import { apiGet } from '@/api/client';

function renderIt(source = 'soroswap') {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(
    <QueryClientProvider client={client}>
      <SourceVolumeHistory source={source} />
    </QueryClientProvider>,
  );
}

describe('isUsdVolumeSeries', () => {
  it('matches the REAL wire name ("USD volume", bespoke_dex.go dexSeriesVolume)', () => {
    expect(isUsdVolumeSeries('USD volume')).toBe(true);
  });

  it('survives cosmetic label edits (case / prefix)', () => {
    expect(isUsdVolumeSeries('Daily USD volume')).toBe(true);
    expect(isUsdVolumeSeries('usd Volume')).toBe(true);
  });

  it('never matches grouped per-pair series or unrelated series', () => {
    expect(isUsdVolumeSeries('Top pairs · XLM/USDC')).toBe(false);
    expect(isUsdVolumeSeries('Trades')).toBe(false);
    expect(isUsdVolumeSeries('Unique traders')).toBe(false);
  });
});

describe('SourceVolumeHistory', () => {
  it('renders the 90d USD-volume chart from the protocol bespoke series (wire name "USD volume")', async () => {
    vi.mocked(apiGet).mockResolvedValue({
      data: {
        bespoke: {
          category: 'dex',
          series: [
            {
              // The REAL wire name — internal/storage/timescale/bespoke_dex.go
              // emits "USD volume". The pre-fix test mocked 'Daily USD volume'
              // and green-lit a panel that rendered null in production.
              name: 'USD volume',
              unit: 'USD',
              points: [
                { date: '2026-07-27', value: '1000.50' },
                { date: '2026-07-28', value: '2000.75' },
              ],
            },
          ],
        },
      },
    });
    renderIt();
    await waitFor(() =>
      expect(screen.getByText('USD volume — 90d')).toBeInTheDocument(),
    );
    expect(vi.mocked(apiGet)).toHaveBeenCalledWith('/v1/protocols/soroswap');
  });

  it('renders nothing when the protocol carries no volume series', async () => {
    vi.mocked(apiGet).mockResolvedValue({ data: { bespoke: { category: 'dex', series: [] } } });
    const { container } = renderIt();
    // Give the query a tick to settle, then assert absence — no chart,
    // no zero-filled placeholder.
    await waitFor(() => expect(vi.mocked(apiGet)).toHaveBeenCalled());
    await waitFor(() => expect(container.firstChild).toBeNull());
  });
});
