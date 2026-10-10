import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { fireEvent, render, screen } from '@testing-library/react';
import { describe, expect, it, vi, beforeEach } from 'vitest';

import type { components } from '@/api/types';

import { RWAHistoryPanel, assetLines, toLine } from './RWAHistoryPanel';

type Schemas = components['schemas'];
type View = Schemas['RWAHistoryView'];
type Point = Schemas['RWAHistoryPoint'];

const apiGetData = vi.hoisted(() => vi.fn());
vi.mock('@/api/client', async () => {
  const actual =
    await vi.importActual<typeof import('@/api/client')>('@/api/client');
  return { ...actual, apiGetData };
});

// Assert on the chart's accessible name: the text a screen reader gets.
vi.mock('@/components/charts/LineChart', () => ({
  LineChart: ({ ariaLabel }: { ariaLabel?: string }) => (
    <div role="img" aria-label={ariaLabel} />
  ),
}));

const ISSUER = 'GCRYUGD5NVARGXT56XEZI5CIFCQETYHAPQQTHO2O3IQZTHDH4LATMYWC';
const DAY2 = '2026-09-11T00:00:00Z';

function point(over: Partial<Point> = {}): Point {
  return {
    t: '2026-09-10T00:00:00Z',
    value_usd: '900000000.00',
    assets_valued: 6,
    assets_unvalued: 5,
    lower_bound: true,
    ...over,
  } as Point;
}

const ends = (a: string, b: string, over: Partial<Point> = {}) => [
  point({ value_usd: a, ...over }),
  point({ t: DAY2, value_usd: b }),
];

function view(over: Partial<View> = {}): View {
  return {
    basis:
      'Circulating supply times the day’s closing value published by an independent oracle.',
    granularity: '1d',
    timeframe: '1y',
    quote: 'fiat:USD',
    assets: 11,
    issuers: 5,
    members: 6,
    membership_as_of: '2026-09-12T09:00:00Z',
    sources: ['redstone'],
    points: ends('900000000.00', '997000000.00'),
    ...over,
  } as View;
}

const group = (code: string, value: string) => ({
  key: `${code}-${ISSUER}`,
  code,
  issuer: ISSUER,
  feed: `rwa:${code}`,
  source: 'redstone',
  members: 1,
  points: [point({ value_usd: value })],
});

type Text = string | RegExp;
const shows = (...t: Text[]) =>
  t.forEach((x) => expect(screen.getByText(x)).toBeInTheDocument());
const hides = (...t: Text[]) =>
  t.forEach((x) => expect(screen.queryByText(x)).not.toBeInTheDocument());

function renderPanel() {
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  });
  return render(
    <QueryClientProvider client={client}>
      <RWAHistoryPanel />
    </QueryClientProvider>,
  );
}

describe('RWAHistoryPanel', () => {
  beforeEach(() => {
    apiGetData.mockReset();
  });

  it('states the change, the floor, the oracle and the controls for a covered window', async () => {
    apiGetData.mockResolvedValue(view());
    renderPanel();
    // 900M → 997M over a constant 6 valued assets.
    expect(await screen.findByText(/\+10\.8%/)).toBeInTheDocument();
    shows(
      /over a constant 6 valued assets/,
      /A floor, not a total/,
      /redstone/,
    );
    shows(/6 of 11 assets in the set carried/, /Not a market capitalisation/);
    shows(/is a gap in the line rather than a zero/);
    shows(/Membership is today’s set applied backwards/);
    expect(screen.getByRole('img')).toHaveAttribute(
      'aria-label',
      expect.stringMatching(/across 6 of 11 assets, a lower bound/),
    );
    for (const name of ['Chart window', 'Series decomposition']) {
      expect(screen.getByRole('group', { name })).toBeInTheDocument();
    }
    for (const name of ['All', 'By asset']) {
      expect(screen.getByRole('button', { name })).toBeInTheDocument();
    }
  });

  it('formats window ends from the exact decimal above 2^53', async () => {
    // Number() of this rounds to 1,000,000.01T.
    const v = '1000000004999999999';
    apiGetData.mockResolvedValue(view({ points: ends(v, v) }));
    renderPanel();
    expect(
      await screen.findByText(/over a constant 6 valued assets/),
    ).toHaveTextContent('$1,000,000T → $1,000,000T');
  });

  it('marks a drop under 0.05% as a fall, not a green +0.0%', async () => {
    apiGetData.mockResolvedValue(
      view({ points: ends('337699858.20', '337559066.79') }),
    );
    renderPanel();
    expect(await screen.findByText(/-0\.0%/)).toHaveClass('text-down');
  });

  it.each([
    [
      'withholds the change when coverage moved across the window',
      view({
        points: ends('900000000.00', '997000000.00', {
          assets_valued: 3,
          assets_unvalued: 8,
        }),
      }),
      [/No change figure/, /their difference is not growth/],
      /across the window/,
    ],
    [
      'says so plainly when the whole set was valued',
      view({
        assets: 6,
        points: [point({ assets_unvalued: 0, lower_bound: false })],
      }),
      [/Full coverage/],
      /A floor, not a total/,
    ],
    [
      'accounts for the members left out of the series',
      view({
        excluded: [
          { reason: 'not_bound', assets: 4, detail: 'No curated binding.' },
          {
            reason: 'contract_not_bound',
            assets: 1,
            detail: 'Contract-issued.',
          },
        ],
      }),
      [/4 with no oracle feed bound to their exact/],
      null,
    ],
    [
      'renders an explanation, never an empty plot, when no series exists',
      view({
        points: [],
        basis: 'No series is published: the reads did not answer.',
      }),
      [/No value series is published/, /the reads did not answer/],
      null,
    ],
    [
      'reports a failed load rather than an empty chart',
      new Error('upstream refused'),
      [/Failed to load the value history/, /upstream refused/],
      null,
    ],
  ])('%s', async (_, answer, present, absent) => {
    if (answer instanceof Error) apiGetData.mockRejectedValue(answer);
    else apiGetData.mockResolvedValue(answer);
    renderPanel();
    await screen.findByText(present[0]);
    shows(...present);
    if (absent) hides(absent);
    if (answer instanceof Error || answer.points.length === 0) {
      expect(screen.queryByRole('img')).not.toBeInTheDocument();
    }
  });

  it('decomposes by asset with a named legend entry per line, not colour alone', async () => {
    apiGetData.mockResolvedValue(
      view({
        groups: [group('USDY', '535000000.00'), group('USTRY', '1300000.00')],
      }),
    );
    renderPanel();
    fireEvent.click(await screen.findByRole('button', { name: 'By asset' }));
    await screen.findByText('USDY');
    shows('USTRY');
    expect(apiGetData).toHaveBeenCalledWith(
      '/v1/rwa/history',
      expect.objectContaining({ group_by: 'asset' }),
    );
  });

  it('asks for the requested window', async () => {
    apiGetData.mockResolvedValue(view());
    renderPanel();
    fireEvent.click(await screen.findByRole('button', { name: 'All' }));
    expect(apiGetData).toHaveBeenCalledWith(
      '/v1/rwa/history',
      expect.objectContaining({ timeframe: 'all' }),
    );
  });
});

describe('toLine — the gaps the server left', () => {
  const iso = (day: number) => `2026-09-${day}T00:00:00Z`;
  const sec = (day: number) => Date.parse(iso(day)) / 1000;
  const at = (day: number, value: string) =>
    point({ t: iso(day), value_usd: value });

  it('emits a null point per missing day, keeping its slot on the time axis', () => {
    const got = toLine([at(10, '10.00'), at(13, '40.00')], false);
    expect(got.map((p) => p.value)).toEqual([10, null, null, 40]);
    expect(got.map((p) => p.time)).toEqual([10, 11, 12, 13].map(sec));
  });

  it('carries coverage into the pane below only when asked', () => {
    expect(toLine([point()], true)[0].volume).toBe(6);
    expect(toLine([point()], false)[0].volume).toBeUndefined();
  });
});

describe('assetLines — colour follows the entity, not the rank', () => {
  const hueOf = (lines: ReturnType<typeof assetLines>, label: string) =>
    lines.find((l) => l.label === label)?.color;

  it('draws largest-first and keeps a hue on its asset when the ranking changes', () => {
    const yearly = assetLines([
      group('USDY', '535000000.00'),
      group('USTRY', '1300000.00'),
    ]);
    const monthly = assetLines([
      group('USTRY', '1300000.00'),
      group('USDY', '900000.00'),
    ]);
    expect(yearly.map((l) => l.label)).toEqual(['USDY', 'USTRY']);
    for (const code of ['USTRY', 'USDY']) {
      expect(hueOf(yearly, code)).toBe(hueOf(monthly, code));
    }
    expect(hueOf(yearly, 'USTRY')).not.toBe(hueOf(yearly, 'USDY'));
  });

  it('never cycles a hue back onto an asset already using it', () => {
    const codes = ['AAA', 'BBB', 'CCC', 'DDD', 'EEE', 'FFF', 'GGG', 'HHH'];
    const hues = assetLines(codes.map((c) => group(c, '1.00'))).map(
      (l) => l.color,
    );
    expect(new Set(hues).size).toBe(hues.length);
  });
});
