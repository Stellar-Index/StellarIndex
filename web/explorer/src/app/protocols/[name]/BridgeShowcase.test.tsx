import { describe, it, expect, vi } from 'vitest';
import { render, screen, waitFor, fireEvent } from '@testing-library/react';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';

import {
  flowLines,
  perChainLines,
  donutSlices,
  chainColor,
} from './BridgeShowcase';
import { BespokeSection } from './BespokeSection';
import type {
  Bespoke,
  BespokeBreakdown,
  BespokeSeries,
} from './BespokeSection';

vi.mock('@/api/client', async () => {
  const actual =
    await vi.importActual<typeof import('@/api/client')>('@/api/client');
  return { ...actual, apiGet: vi.fn() };
});

// The canvas chart can't run under jsdom; record its series composition.
vi.mock('@/components/charts/LineChart', () => ({
  LineChart: (props: {
    data: unknown[];
    series?: { label: string; data: unknown[] }[];
    timeVisible?: boolean;
    ariaLabel?: string;
  }) => (
    <div
      data-testid="line-chart"
      role="img"
      aria-label={props.ariaLabel}
      data-points={props.data.length}
      data-series={(props.series ?? [])
        .map((s) => `${s.label}:${s.data.length}`)
        .join('|')}
      data-timevisible={String(props.timeVisible ?? false)}
    />
  ),
}));

import { apiGet, asExample } from '@/api/client';

const TX = '71689b2f79215976f6099f0c705b3ffb4ff8f2f40d1ee2b43636c699e71fbffe';
type Text = string | RegExp;
const shows = (...t: Text[]) =>
  t.forEach((x) => expect(screen.getByText(x)).toBeInTheDocument());
const hides = (...t: Text[]) =>
  t.forEach((x) => expect(screen.queryByText(x)).not.toBeInTheDocument());

const s = (name: string, ...pts: [string, string][]): BespokeSeries => ({
  name,
  unit: 'USDC',
  points: pts.map(([date, value]) => ({ date, value })),
});

const breakdown = (
  rows: [string, string][],
  title = 'Inflows by source chain',
): BespokeBreakdown => ({
  title,
  unit: 'USDC',
  rows: rows.map(([label, value]) => ({ label, value, count: 1 })),
});

const cctpBespoke: Bespoke = {
  category: 'bridge',
  series: [
    s('Inbound (USDC)', ['2026-07-27', '1000.50'], ['2026-07-28', '2000.25']),
    s('Outbound (USDC)', ['2026-07-27', '500.10'], ['2026-07-28', '750.00']),
    s('Inbound · Base', ['2026-07-27', '600.00'], ['2026-07-28', '900.00']),
    s('Inbound · Solana', ['2026-07-28', '400.00']),
    s('Outbound · Ethereum', ['2026-07-28', '750.00']),
    s(
      'Cumulative net inflow (all-time)',
      ['2026-01-01', '100.00'],
      ['2026-07-28', '8003390.36'],
    ),
  ],
  breakdowns: [
    breakdown([
      ['Base', '6432257.50'],
      ['Ethereum', '2742082.73'],
      ['Solana', '2001146.57'],
      ['Unverified (0x3600…0000)', '0.01'],
    ]),
    breakdown(
      [
        ['Solana', '1802090.41'],
        ['Ethereum', '1107891.94'],
      ],
      'Outflows by destination chain',
    ),
  ],
  tables: [
    {
      title: 'Largest transfers',
      columns: ['Direction', 'Chain', 'Amount (USDC)', 'Tx', 'Date'],
      rows: [['Inbound', 'Ethereum', '901413.243537', TX, '2026-06-11']],
    },
  ],
};

const rozoBespoke: Bespoke = {
  category: 'bridge',
  series: [s('Settled volume (USDC)', ['2026-07-28', '42.5'])],
};

function renderIt(name: string, initial: Bespoke) {
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  });
  return render(
    <QueryClientProvider client={client}>
      <BespokeSection
        bespoke={initial}
        name={name}
        source={asExample(`/v1/protocols/${name}`)}
      />
    </QueryClientProvider>,
  );
}

const charts = () => screen.queryAllByTestId('line-chart');
const chartWith = (series: string) =>
  charts().find((c) => c.getAttribute('data-series') === series);

describe('flowLines', () => {
  it.each([
    [
      'pairs the exact-name totals, ignoring per-chain and cumulative series',
      cctpBespoke.series!,
      ['Inbound (USDC)', 'Outbound (USDC)'],
    ],
    [
      'renders a lone series as one line',
      rozoBespoke.series!,
      ['Settled volume (USDC)'],
    ],
    [
      'never falls back to an auxiliary series',
      [
        s('Inbound · Base', ['2026-07-28', '1']),
        s('Cumulative net inflow (all-time)', ['2026-07-28', '2']),
      ],
      [],
    ],
  ])('%s', (_, series, labels) => {
    expect(flowLines(series).map((l) => l.label)).toEqual(labels);
  });

  it('gives the two directions distinct tones', () => {
    const [inb, out] = flowLines(cctpBespoke.series!);
    expect(inb.tone).not.toBe(out.tone);
  });

  it('parses hourly timestamps 3600s apart', () => {
    const [inb] = flowLines([
      s('Inbound (USDC)', ['2026-07-29T13:00', '1'], ['2026-07-29T14:00', '2']),
      s('Outbound (USDC)', ['2026-07-29T13:00', '3']),
    ]);
    expect(inb.data[1].time - inb.data[0].time).toBe(3600);
  });
});

describe('perChainLines', () => {
  it.each([
    ['Inbound · ', ['Base', 'Solana']],
    ['Outbound · ', ['Ethereum']],
  ])('extracts %j series as chain-labelled lines', (prefix, labels) => {
    const lines = perChainLines(cctpBespoke.series!, prefix);
    expect(lines.map((l) => l.label)).toEqual(labels);
    for (const l of lines) expect(l.color).toBe(chainColor(l.label));
  });

  it('never gives two chains one colour', () => {
    expect(chainColor('Base')).not.toBe(chainColor('Solana'));
  });
});

describe('donutSlices', () => {
  it('keeps honest unknown labels as their own slice', () => {
    expect(donutSlices(cctpBespoke.breakdowns![0]).map((x) => x.label)).toEqual(
      ['Base', 'Ethereum', 'Solana', 'Unverified (0x3600…0000)'],
    );
  });

  const fold = (top: (i: number) => number, rest: string[]) =>
    donutSlices(
      breakdown([
        ...Array.from({ length: 6 }, (_, i): [string, string] => [
          `Chain${i}`,
          String(top(i)),
        ]),
        ...rest.map((v, i): [string, string] => [`Rest${i}`, v]),
      ]),
    );

  it.each([
    [
      'folds slices beyond the top 6 into an Others bucket',
      (i: number) => 900 - i * 100,
      ['300', '200', '100'],
      { label: 'Others (3)', value: 600 },
    ],
    // A float reduce would give 27021597764222984.
    [
      'sums the Others fold exactly above 2^53',
      (i: number) => 9e18 - i * 1e18,
      ['9007199254740993', '9007199254740995', '9007199254740997'],
      { label: 'Others (3)', decimal: '27021597764222985' },
    ],
    // "50." parses via Number() but not as a plain decimal: void, never drop.
    [
      'voids the exact Others total when a row fails the decimal parser',
      (i: number) => 700 - i * 100,
      ['100', '50.'],
      { label: 'Others (2)', value: 150, decimal: null },
    ],
  ])('%s', (_, top, rest, others) => {
    const slices = fold(top, rest);
    expect(slices).toHaveLength(7);
    expect(slices[6]).toMatchObject(others);
  });
});

describe('BridgeShowcase', () => {
  it('renders the full cctp suite from the initial data', async () => {
    renderIt('cctp', cctpBespoke);

    for (const label of ['24h', '7d', '30d', '90d']) {
      expect(screen.getByRole('button', { name: label })).toHaveAttribute(
        'aria-pressed',
        String(label === '90d'),
      );
    }
    await waitFor(() => expect(charts().length).toBeGreaterThan(0));

    const cumulative = charts().find(
      (c) => c.getAttribute('data-points') === '2',
    );
    expect(cumulative).toHaveAttribute(
      'aria-label',
      expect.stringMatching(/Cumulative net USDC inflow/),
    );
    for (const series of [
      'Inbound (USDC):2|Outbound (USDC):2',
      'Base:2|Solana:1',
      'Ethereum:1',
    ]) {
      expect(chartWith(series)).toBeDefined();
    }
    // 57.6% is Base's share of 6432257.50 / 11175486.81.
    shows('Cumulative net inflow', 'Inflows by source chain', 'Where funds go');
    shows('Outflows by destination chain', 'Where funds come from', '57.6%');
    shows('Unverified (0x3600…0000)', 'Largest transfers');
    const hrefs = screen
      .getAllByRole('link')
      .map((a) => a.getAttribute('href'));
    expect(hrefs.some((h) => h?.startsWith(`/transactions/${TX}`))).toBe(true);
    // The 90d default reuses the page's data.
    expect(vi.mocked(apiGet)).not.toHaveBeenCalled();
  });

  it('refetches on pill click, goes hourly, and keeps the all-time cumulative headline', async () => {
    vi.mocked(apiGet).mockResolvedValue({
      data: {
        bespoke: {
          category: 'bridge',
          series: [
            s(
              'Inbound (USDC)',
              ['2026-07-29T13:00', '10'],
              ['2026-07-29T14:00', '20'],
              ['2026-07-29T15:00', '30'],
            ),
            s('Outbound (USDC)', ['2026-07-29T13:00', '5']),
            s(
              'Cumulative net inflow (all-time)',
              ['2026-01-01', '100.00'],
              ['2026-07-28', '8003390.36'],
              ['2026-07-29', '8003400.00'],
            ),
          ],
        },
      },
    });
    renderIt('cctp', cctpBespoke);

    fireEvent.click(screen.getByRole('button', { name: '24h' }));
    await waitFor(() =>
      expect(vi.mocked(apiGet)).toHaveBeenCalledWith(
        '/v1/protocols/cctp?days=1',
      ),
    );
    await waitFor(() =>
      expect(chartWith('Inbound (USDC):3|Outbound (USDC):1')).toHaveAttribute(
        'data-timevisible',
        'true',
      ),
    );
    expect(screen.getByRole('button', { name: '24h' })).toHaveAttribute(
      'aria-pressed',
      'true',
    );
    expect(charts().some((c) => c.getAttribute('data-points') === '2')).toBe(
      true,
    );
  });

  it('shows an honest empty state when the window has no transfers', async () => {
    vi.mocked(apiGet).mockResolvedValue({
      data: { bespoke: { category: 'bridge', series: [] } },
    });
    renderIt('rozo', rozoBespoke);

    fireEvent.click(screen.getByRole('button', { name: '7d' }));
    expect(
      await screen.findByText('No transfers in this window.'),
    ).toBeInTheDocument();
    expect(vi.mocked(apiGet)).toHaveBeenCalledWith('/v1/protocols/rozo?days=7');
    expect(charts()).toHaveLength(0);
  });

  it('renders rozo as one settled-volume line without donuts or cumulative', async () => {
    renderIt('rozo', rozoBespoke);
    await waitFor(() => expect(charts()).toHaveLength(1));
    expect(charts()[0]).toHaveAttribute(
      'data-series',
      'Settled volume (USDC):1',
    );
    hides('Where funds come from', 'Cumulative net inflow');
  });
});
