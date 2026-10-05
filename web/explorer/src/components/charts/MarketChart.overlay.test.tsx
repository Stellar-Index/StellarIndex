import { fireEvent, render, screen, waitFor } from '@testing-library/react';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { MarketChart, overlayPoints, selectableSources } from './MarketChart';

const lw = vi.hoisted(() => ({ setData: vi.fn() }));
vi.mock('lightweight-charts', async (importOriginal) => ({
  ...(await importOriginal<typeof import('lightweight-charts')>()),
  createChart: () => ({
    addSeries: () => ({
      setData: lw.setData,
      applyOptions: () => {},
      createPriceLine: () => ({ applyOptions: () => {} }),
      removePriceLine: () => {},
      priceScale: () => ({ applyOptions: () => {} }),
    }),
    timeScale: () => ({ fitContent: () => {} }),
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

const sources = [
  { name: 'sdex', selectable: true, on_chain: true },
  { name: 'binance', selectable: true, on_chain: false },
  { name: 'coingecko', selectable: false, on_chain: false },
];

let fetched: string[] = [];
beforeEach(() => {
  fetched = [];
  vi.stubGlobal(
    'ResizeObserver',
    class {
      observe() {}
      disconnect() {}
    },
  );
  vi.stubGlobal(
    'fetch',
    vi.fn(async (input: RequestInfo | URL) => {
      const url = String(input);
      fetched.push(url);
      const body = url.includes('/v1/sources')
        ? { data: sources }
        : url.includes('/v1/history')
          ? { data: [] }
          : { data: { intervals: [] } };
      return { ok: true, status: 200, json: async () => body };
    }),
  );
});
afterEach(() => vi.unstubAllGlobals());

describe('overlay helpers', () => {
  it('offers only selectable on-chain sources', () => {
    expect(selectableSources(sources as never).map((s) => s.name)).toEqual([
      'sdex',
    ]);
  });

  it('plots served prices and skips rows without one', () => {
    const row = (price: string | null, ts: string) =>
      ({ price, ts }) as Parameters<typeof overlayPoints>[0][number];
    expect(
      overlayPoints([
        row('0.25', '2026-07-03T20:00:00Z'),
        row(null, '2026-07-03T20:00:01Z'),
        row('0', '2026-07-03T20:00:02Z'),
      ]),
    ).toEqual([{ time: Date.UTC(2026, 6, 3, 20) / 1000, value: 0.25 }]);
  });
});

function renderChart(sourceOverlay: boolean) {
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  });
  render(
    <QueryClientProvider client={client}>
      <MarketChart
        base="native"
        quote="fiat:USD"
        baseLabel="XLM"
        quoteLabel="USD"
        sourceOverlay={sourceOverlay}
      />
    </QueryClientProvider>,
  );
}

describe('MarketChart source overlay', () => {
  it('is absent unless the surface opts in', () => {
    renderChart(false);
    expect(
      screen.queryByLabelText('Overlay trades from one source'),
    ).not.toBeInTheDocument();
    expect(fetched.some((u) => u.includes('/v1/sources'))).toBe(false);
  });

  it('requests /v1/history with the chosen source only after selection', async () => {
    renderChart(true);
    const select = await screen.findByLabelText(
      'Overlay trades from one source',
    );
    expect(fetched.some((u) => u.includes('/v1/history'))).toBe(false);
    fireEvent.change(select, { target: { value: 'sdex' } });
    await waitFor(() =>
      expect(
        fetched.some(
          (u) => u.includes('/v1/history') && u.includes('source=sdex'),
        ),
      ).toBe(true),
    );
  });
});
