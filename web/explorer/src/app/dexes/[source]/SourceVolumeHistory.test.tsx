import { describe, it, expect, vi } from 'vitest';
import { render, screen, waitFor } from '@testing-library/react';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';

import { SourceVolumeHistory } from './SourceVolumeHistory';

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

describe('SourceVolumeHistory', () => {
  it('renders the 90d USD-volume chart from the protocol bespoke series', async () => {
    vi.mocked(apiGet).mockResolvedValue({
      data: {
        bespoke: {
          category: 'dex',
          series: [
            {
              name: 'Daily USD volume',
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
