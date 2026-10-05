import {
  fireEvent,
  render,
  screen,
  waitFor,
  within,
} from '@testing-library/react';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { CandleChart } from './CandleChart';
import { MarketChart } from './MarketChart';

// Canvas charting has no jsdom surface: record the calls the chart receives.
const lw = vi.hoisted(() => ({
  setData: vi.fn(),
  fitContent: vi.fn(),
  createPriceLine: vi.fn(() => ({ applyOptions: () => {} })),
}));
vi.mock('lightweight-charts', async (importOriginal) => ({
  ...(await importOriginal<typeof import('lightweight-charts')>()),
  createChart: () => ({
    addSeries: () => ({
      setData: lw.setData,
      applyOptions: () => {},
      createPriceLine: lw.createPriceLine,
      removePriceLine: () => {},
      priceScale: () => ({ applyOptions: () => {} }),
    }),
    timeScale: () => ({ fitContent: lw.fitContent }),
    panes: () => [],
    applyOptions: () => {},
    remove: () => {},
  }),
}));

vi.mock('@/lib/live/hooks', async (importOriginal) => ({
  ...(await importOriginal<typeof import('@/lib/live/hooks')>()),
  useLedgerFollow: () => undefined,
  useTipStream: () => null,
  useLiveClock: () => Date.now(),
}));

beforeEach(() =>
  vi.stubGlobal(
    'fetch',
    vi.fn().mockResolvedValue({
      ok: true,
      status: 200,
      json: async () => ({ data: { intervals: [] } }),
    }),
  ),
);
beforeEach(() => {
  vi.stubGlobal(
    'ResizeObserver',
    class {
      observe() {}
      disconnect() {}
    },
  );
  lw.setData.mockClear();
  lw.fitContent.mockClear();
  lw.createPriceLine.mockClear();
});
afterEach(() => vi.unstubAllGlobals());

// 15m bars (the 7d default grain) over 3h, so a 1h band has points.
const bars = Array.from({ length: 12 }, (_, i) => ({
  t: new Date(Date.UTC(2026, 6, 3, 20) + i * 900_000).toISOString(),
  o: '1',
  h: String(1 + i / 100),
  l: String(1 - i / 100),
  c: '1',
  v_base: '1',
  v_quote: '1',
  v_base_decimals: 7,
  v_quote_decimals: null,
  n: 1,
}));

function renderChart(volatilityBand: boolean) {
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  });
  const ui = () => (
    <QueryClientProvider client={client}>
      <MarketChart
        base="native"
        quote="fiat:USD"
        baseLabel="XLM"
        quoteLabel="USD"
        volatilityBand={volatilityBand}
      />
    </QueryClientProvider>
  );
  const { rerender } = render(ui());
  return () => rerender(ui());
}

function bandLabels(): string[] {
  const group = screen.getByRole('group', { name: 'Volatility band' });
  return within(group)
    .getAllByRole('button')
    .map((b) => b.textContent ?? '');
}

describe('MarketChart volatility band', () => {
  it('is absent unless the surface opts in', () => {
    renderChart(false);
    expect(
      screen.queryByRole('group', { name: 'Volatility band' }),
    ).not.toBeInTheDocument();
  });

  it('offers only windows longer than the candle', () => {
    renderChart(true);
    expect(bandLabels()).toEqual(['No band', '1h band', '4h band', '24h band']);

    const interval = screen.getByRole('group', { name: 'Candle interval' });
    fireEvent.click(within(interval).getByRole('button', { name: '4h' }));
    expect(bandLabels()).toEqual(['No band', '24h band']);

    const window = screen.getByRole('group', { name: 'Chart window' });
    fireEvent.click(within(window).getByRole('button', { name: '1y' }));
    expect(
      screen.queryByRole('group', { name: 'Volatility band' }),
    ).not.toBeInTheDocument();
  });

  it('keeps the chart viewport across a re-render with the band on', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn().mockResolvedValue({
        ok: true,
        status: 200,
        json: async () => ({ data: { intervals: bars } }),
      }),
    );
    const rerender = renderChart(true);
    const group = screen.getByRole('group', { name: 'Volatility band' });
    fireEvent.click(within(group).getByRole('button', { name: '1h band' }));
    await screen.findByRole('img', { name: /1h high\/low band/ });
    await waitFor(() => expect(lw.fitContent).toHaveBeenCalled());

    const fits = lw.fitContent.mock.calls.length;
    const sets = lw.setData.mock.calls.length;
    rerender();
    expect(lw.fitContent).toHaveBeenCalledTimes(fits);
    expect(lw.setData).toHaveBeenCalledTimes(sets);
  });

  it('redraws the live price line when toggling the band rebuilds the chart', () => {
    const data = [{ time: 1, open: 1, high: 2, low: 0.5, close: 1 }];
    const band = [{ time: 1, upper: 2, lower: 0.5 }];
    const { rerender } = render(<CandleChart data={data} livePrice={1.5} />);
    expect(lw.createPriceLine).toHaveBeenCalledTimes(1);

    rerender(<CandleChart data={data} livePrice={1.5} band={band} />);
    expect(lw.createPriceLine).toHaveBeenCalledTimes(2);
  });
});

describe('MarketChart fetch bound', () => {
  it('aborts a hung OHLC request instead of loading forever', async () => {
    vi.useFakeTimers({ toFake: ['setTimeout', 'clearTimeout'] });
    try {
      let seen: AbortSignal | undefined;
      vi.stubGlobal(
        'fetch',
        vi.fn((_u: string, init?: RequestInit) => {
          seen = init?.signal ?? undefined;
          return new Promise(() => {});
        }),
      );
      renderChart(false);
      await vi.waitFor(() => expect(seen).toBeDefined());
      expect(seen?.aborted).toBe(false);
      await vi.advanceTimersByTimeAsync(60_000);
      expect(seen?.aborted).toBe(true);
    } finally {
      vi.useRealTimers();
    }
  });
});
