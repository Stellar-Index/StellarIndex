import { describe, it, expect, vi } from 'vitest';
import { render, screen, waitFor } from '@testing-library/react';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';

import { SourceHealthPanel } from './SourceHealthPanel';
import { VOLUME_FLOOR_HINT } from '@/lib/supplyFloor';

// On an explorer-ahead-of-API deploy the /v1/sources/{name}/health
// response can omit `entries_24h` (the field predates the older API). The
// unguarded `data.entries_24h.toLocaleString()` throws on the missing value,
// which trips the segment error boundary and blanks the panel. The guarded
// render must degrade to the em-dash absent-value idiom instead of throwing.
vi.mock('@/api/client', async () => {
  const actual =
    await vi.importActual<typeof import('@/api/client')>('@/api/client');
  return { ...actual, apiGet: vi.fn() };
});

import { apiGet } from '@/api/client';

describe('SourceHealthPanel — deploy-skew resilience', () => {
  it('renders "—" for a health row that omits entries_24h (no render throw)', async () => {
    // restoreMocks: true wipes any factory-time impl, so configure here.
    vi.mocked(apiGet).mockResolvedValue({
      // entries_24h intentionally absent — simulates an API older than the
      // explorer, from before the field existed.
      data: {
        name: 'binance',
        class: 'exchange',
        trade_count_24h: 5,
        markets_count_24h: 3,
        volume_24h_usd: '1000',
      },
    });
    const client = new QueryClient({
      defaultOptions: { queries: { retry: false } },
    });
    render(
      <QueryClientProvider client={client}>
        <SourceHealthPanel source="binance" />
      </QueryClientProvider>,
    );
    // An absent counter is unknown, not a finding of "no events". Reaching
    // this assertion proves the panel rendered (an unguarded render throws).
    await waitFor(() =>
      expect(screen.getByText('not reported by this API')).toBeInTheDocument(),
    );
    expect(screen.getByText('—')).toBeInTheDocument();
    expect(
      screen.queryByText('no decoded events in 24h'),
    ).not.toBeInTheDocument();
  });

  // The API soft-fails a timed-out stats or Prometheus read to 0 with no
  // flag, so a 0 counter cannot be shown as a measured "0 / dead venue".
  it('never presents a zero counter as a measured zero', async () => {
    vi.mocked(apiGet).mockResolvedValue({
      data: {
        name: 'sdex',
        class: 'exchange',
        enabled: true,
        trade_count_24h: 0,
        markets_count_24h: 0,
        entries_24h: 0,
      },
    });
    const client = new QueryClient({
      defaultOptions: { queries: { retry: false } },
    });
    render(
      <QueryClientProvider client={client}>
        <SourceHealthPanel source="sdex" />
      </QueryClientProvider>,
    );
    await waitFor(() =>
      expect(
        screen.getAllByText('none counted, or the read failed'),
      ).toHaveLength(3),
    );
    expect(screen.queryByText('0')).not.toBeInTheDocument();
    expect(
      screen.queryByText('no decoded events in 24h'),
    ).not.toBeInTheDocument();
  });

  it('labels a switched-off source as off, not as failing', async () => {
    vi.mocked(apiGet).mockResolvedValue({
      data: {
        name: 'kraken',
        class: 'exchange',
        enabled: false,
        trade_count_24h: 0,
        markets_count_24h: 0,
        entries_24h: 0,
      },
    });
    const client = new QueryClient({
      defaultOptions: { queries: { retry: false } },
    });
    render(
      <QueryClientProvider client={client}>
        <SourceHealthPanel source="kraken" />
      </QueryClientProvider>,
    );
    expect(await screen.findByText('Off')).toBeInTheDocument();
    expect(
      screen.getByText('not switched on for this deployment'),
    ).toBeInTheDocument();
  });

  it('renders 24h volume exactly from its decimal string', async () => {
    vi.mocked(apiGet).mockResolvedValue({
      data: {
        name: 'binance',
        class: 'exchange',
        trade_count_24h: 5,
        markets_count_24h: 3,
        entries_24h: 5,
        // As a float this rounds up to 499.995 and renders $500.
        volume_24h_usd: '499.994999999999999999',
      },
    });
    const client = new QueryClient({
      defaultOptions: { queries: { retry: false } },
    });
    render(
      <QueryClientProvider client={client}>
        <SourceHealthPanel source="binance" />
      </QueryClientProvider>,
    );
    expect(await screen.findByText('$499.99')).toBeInTheDocument();
  });

  it('renders a lower-bound volume as a floor and an exact one without', async () => {
    const row = {
      name: 'soroswap',
      class: 'exchange',
      trade_count_24h: 10,
      markets_count_24h: 3,
      entries_24h: 10,
      volume_24h_usd: '120.5',
    };
    for (const [flag, text] of [
      [true, '≥ $120.5'],
      [false, '$120.5'],
    ] as const) {
      vi.mocked(apiGet).mockResolvedValue({
        data: flag ? { ...row, volume_lower_bound: true } : row,
      });
      const client = new QueryClient({
        defaultOptions: { queries: { retry: false } },
      });
      const { unmount } = render(
        <QueryClientProvider client={client}>
          <SourceHealthPanel source="soroswap" />
        </QueryClientProvider>,
      );
      const value = await screen.findByText(text);
      if (flag) expect(value).toHaveAttribute('title', VOLUME_FLOOR_HINT);
      else expect(value).not.toHaveAttribute('title');
      unmount();
    }
  });
});
