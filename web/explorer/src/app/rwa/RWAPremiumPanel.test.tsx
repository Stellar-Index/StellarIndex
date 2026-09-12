import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { render, screen } from '@testing-library/react';
import { describe, expect, it, vi, beforeEach } from 'vitest';

import type { components } from '@/api/types';

import { RWAPremiumPanel, premiumLines } from './RWAPremiumPanel';

// The panel draws one line for a handful of the set and leaves holes
// everywhere else. Both of those are ways a reader gets misled — the
// line taken for the state of the sector, and a hole drawn through as
// if it were data — so both are what these tests are about.

type Schemas = components['schemas'];
type View = Schemas['RWAPremiumHistoryView'];
type Series = Schemas['RWAPremiumSeries'];
type Point = Schemas['RWAPremiumHistoryPoint'];

const apiGetData = vi.hoisted(() => vi.fn());
vi.mock('@/api/client', async () => {
  const actual =
    await vi.importActual<typeof import('@/api/client')>('@/api/client');
  return { ...actual, apiGetData };
});

// The canvas chart is client-only and never mounts under jsdom in a way
// worth asserting on; stub it and capture both the accessible name it
// is handed and the reference lines, since the zero line is part of the
// chart's meaning rather than its decoration.
const chartProps = vi.hoisted(() => ({ current: null as unknown }));
vi.mock('@/components/charts/LineChart', () => ({
  LineChart: (props: { ariaLabel?: string }) => {
    chartProps.current = props;
    return <div role="img" aria-label={props.ariaLabel} />;
  },
}));

const ISSUER = 'GCRYUGD5NVARGXT56XEZI5CIFCQETYHAPQQTHO2O3IQZTHDH4LATMYWC';

function point(over: Partial<Point> = {}): Point {
  return {
    t: '2026-09-10T00:00:00Z',
    premium_pct: '0.9346',
    market_usd: '1.0800000000',
    reference_usd: '1.0700000000',
    volume_usd: '18420.55',
    trades: 37,
    ...over,
  } as Point;
}

function series(over: Partial<Series> = {}): Series {
  return {
    asset_id: `CETES-${ISSUER}`,
    code: 'CETES',
    issuer: ISSUER,
    label: 'Etherfuse CETES',
    feed: 'rwa:CETES',
    source: 'redstone',
    points: [
      point(),
      point({ t: '2026-09-11T00:00:00Z', premium_pct: '-0.4' }),
    ],
    ...over,
  } as Series;
}

function view(over: Partial<View> = {}): View {
  return {
    basis:
      'The token’s own observed dollar price on a day, measured against what an independent oracle published that same day.',
    granularity: '1d',
    timeframe: '1y',
    quote: 'fiat:USD',
    assets: 11,
    issuers: 5,
    bound: 7,
    members: 1,
    membership_as_of: '2026-09-12T09:00:00Z',
    sources: ['redstone'],
    series: [series()],
    coverage: [
      { t: '2026-09-10T00:00:00Z', assets_measured: 1, assets_unmeasured: 10 },
      { t: '2026-09-11T00:00:00Z', assets_measured: 1, assets_unmeasured: 10 },
    ],
    ...over,
  } as View;
}

function renderPanel() {
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  });
  return render(
    <QueryClientProvider client={client}>
      <RWAPremiumPanel />
    </QueryClientProvider>,
  );
}

describe('RWAPremiumPanel', () => {
  beforeEach(() => {
    apiGetData.mockReset();
    chartProps.current = null;
  });

  it('says how much of the set the line actually covers', async () => {
    apiGetData.mockResolvedValue(view());
    renderPanel();
    expect((await screen.findAllByText(/1 of 11/)).length).toBeGreaterThan(0);
    expect(
      screen.getByText(/7 carry a curated binding to an instrument feed/),
    ).toBeInTheDocument();
  });

  it('states that a silent day is a break, not a flat stretch', async () => {
    apiGetData.mockResolvedValue(view());
    renderPanel();
    expect(
      await screen.findByText(/a break in the line, never a flat stretch/),
    ).toBeInTheDocument();
  });

  it('reports the days it saw trades on and refused to price', async () => {
    apiGetData.mockResolvedValue(
      view({ series: [series({ market_withheld_days: 12 })] }),
    );
    renderPanel();
    expect(
      await screen.findByText(/12 days carried observed trades/),
    ).toBeInTheDocument();
    expect(
      screen.getByText(/did not clear the thin-market floor/),
    ).toBeInTheDocument();
  });

  it('accounts for the members with no series at all', async () => {
    apiGetData.mockResolvedValue(
      view({
        excluded: [
          { reason: 'no_market_history', assets: 4, detail: 'never traded' },
          { reason: 'not_bound', assets: 4, detail: 'no binding' },
        ],
      }),
    );
    renderPanel();
    expect(
      await screen.findByText(/4 that have never traded against a dollar/),
    ).toBeInTheDocument();
    expect(
      screen.getByText(
        /4 with no oracle feed bound to their exact \(code, issuer\)/,
      ),
    ).toBeInTheDocument();
  });

  it('draws a zero line, because the sign is the finding', async () => {
    apiGetData.mockResolvedValue(view());
    renderPanel();
    await screen.findByRole('img');
    const props = chartProps.current as { priceLines?: { value: number }[] };
    expect(props.priceLines).toEqual([
      expect.objectContaining({ value: 0, label: 'par' }),
    ]);
  });

  it('renders an explanation, never an empty plot, when nothing is comparable', async () => {
    apiGetData.mockResolvedValue(view({ series: [], coverage: [] }));
    renderPanel();
    expect(
      await screen.findByText(/No premium series is published/),
    ).toBeInTheDocument();
    expect(screen.queryByRole('img')).not.toBeInTheDocument();
  });

  it('reports a failed load rather than an empty chart', async () => {
    apiGetData.mockRejectedValue(new Error('upstream said no'));
    renderPanel();
    expect(
      await screen.findByText(/Failed to load the premium history/),
    ).toBeInTheDocument();
  });

  it('names the instrument in text, so identity never rests on colour', async () => {
    apiGetData.mockResolvedValue(view());
    renderPanel();
    const legend = await screen.findAllByText('CETES');
    expect(legend.length).toBeGreaterThan(0);
  });

  it('carries the coverage into the chart’s text alternative', async () => {
    apiGetData.mockResolvedValue(view());
    renderPanel();
    const chart = await screen.findByRole('img');
    expect(chart.getAttribute('aria-label')).toMatch(/1 of 11/);
    expect(chart.getAttribute('aria-label')).toMatch(/Positive is a premium/);
  });

  it('asks for the requested window', async () => {
    apiGetData.mockResolvedValue(view());
    renderPanel();
    await screen.findByRole('img');
    expect(apiGetData).toHaveBeenCalledWith('/v1/rwa/premium', {
      timeframe: '1y',
    });
  });
});

describe('premiumLines — the gaps and the hues', () => {
  it('breaks the line at a day neither leg was observed on', () => {
    const [line] = premiumLines([
      series({
        points: [
          point({ t: '2026-09-10T00:00:00Z', premium_pct: '1' }),
          // 2026-09-11 is missing entirely.
          point({ t: '2026-09-12T00:00:00Z', premium_pct: '2' }),
        ],
      }),
    ]);
    expect(line.data).toHaveLength(3);
    expect(line.data[1].value).toBeNull();
  });

  it('keeps a hue on its instrument when the ranking changes', () => {
    const a = series({ asset_id: 'AAA', code: 'AAA' });
    const b = series({ asset_id: 'BBB', code: 'BBB' });
    const hue = (lines: ReturnType<typeof premiumLines>, label: string) =>
      lines.find((l) => l.label === label)?.color;

    const oneOrder = premiumLines([a, b]);
    const theOther = premiumLines([b, a]);
    expect(hue(oneOrder, 'AAA')).toBe(hue(theOther, 'AAA'));
    expect(hue(oneOrder, 'BBB')).toBe(hue(theOther, 'BBB'));
    expect(hue(oneOrder, 'AAA')).not.toBe(hue(oneOrder, 'BBB'));
  });

  it('draws in the server’s order, widest dispersion first', () => {
    const lines = premiumLines([
      series({ asset_id: 'ZZZ', code: 'ZZZ' }),
      series({ asset_id: 'AAA', code: 'AAA' }),
    ]);
    expect(lines.map((l) => l.label)).toEqual(['ZZZ', 'AAA']);
  });

  it('drops a series the window left empty rather than drawing a bare axis', () => {
    const lines = premiumLines([series({ points: [] })]);
    expect(lines).toHaveLength(0);
  });
});
