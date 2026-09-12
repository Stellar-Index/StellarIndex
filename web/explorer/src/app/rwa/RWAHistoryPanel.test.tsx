import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { fireEvent, render, screen } from '@testing-library/react';
import { describe, expect, it, vi, beforeEach } from 'vitest';

import type { components } from '@/api/types';

import { RWAHistoryPanel } from './RWAHistoryPanel';

// The panel's job is to keep a reader from misreading the line. The
// tests below are almost entirely about the two ways that happens: a
// number stated across endpoints with different coverage, and a total
// read as the value of the sector when it is a floor.

type Schemas = components['schemas'];
type View = Schemas['RWAHistoryView'];
type Point = Schemas['RWAHistoryPoint'];

const apiGetData = vi.hoisted(() => vi.fn());
vi.mock('@/api/client', async () => {
  const actual =
    await vi.importActual<typeof import('@/api/client')>('@/api/client');
  return { ...actual, apiGetData };
});

// The canvas chart is client-only and never mounts under jsdom in a way
// worth asserting on; stub it and assert on the accessible name it is
// handed, which is the text alternative a screen reader actually gets.
vi.mock('@/components/charts/LineChart', () => ({
  LineChart: ({ ariaLabel }: { ariaLabel?: string }) => (
    <div role="img" aria-label={ariaLabel} />
  ),
}));

const ISSUER = 'GCRYUGD5NVARGXT56XEZI5CIFCQETYHAPQQTHO2O3IQZTHDH4LATMYWC';

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
    points: [
      point(),
      point({ t: '2026-09-11T00:00:00Z', value_usd: '997000000.00' }),
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
      <RWAHistoryPanel />
    </QueryClientProvider>,
  );
}

describe('RWAHistoryPanel', () => {
  beforeEach(() => {
    apiGetData.mockReset();
  });

  it('states the window change only when both ends cover the same assets', async () => {
    apiGetData.mockResolvedValue(view());
    renderPanel();
    // 900M → 997M over a constant 6 valued assets is +10.8%.
    expect(await screen.findByText(/\+10\.8%/)).toBeInTheDocument();
    expect(
      screen.getByText(/over a constant 6 valued assets/),
    ).toBeInTheDocument();
  });

  it('WITHHOLDS the change when coverage moved across the window', async () => {
    apiGetData.mockResolvedValue(
      view({
        points: [
          point({ assets_valued: 3, assets_unvalued: 8 }),
          point({
            t: '2026-09-11T00:00:00Z',
            value_usd: '997000000.00',
            assets_valued: 6,
            assets_unvalued: 5,
          }),
        ],
      }),
    );
    renderPanel();
    expect(await screen.findByText(/No change figure/)).toBeInTheDocument();
    expect(
      screen.getByText(/their difference is not growth/),
    ).toBeInTheDocument();
    expect(screen.queryByText(/across the window/)).not.toBeInTheDocument();
  });

  it('says the total is a floor, with the coverage of the latest day', async () => {
    apiGetData.mockResolvedValue(view());
    renderPanel();
    expect(await screen.findByText(/A floor, not a total/)).toBeInTheDocument();
    expect(
      screen.getByText(/6 of 11 assets in the set carried/),
    ).toBeInTheDocument();
    expect(
      screen.getByText(/is a gap in the line rather than a zero/),
    ).toBeInTheDocument();
  });

  it('says so plainly when the whole set was valued', async () => {
    apiGetData.mockResolvedValue(
      view({
        assets: 6,
        points: [point({ assets_unvalued: 0, lower_bound: false })],
      }),
    );
    renderPanel();
    expect(await screen.findByText(/Full coverage/)).toBeInTheDocument();
    expect(screen.queryByText(/A floor, not a total/)).not.toBeInTheDocument();
  });

  it('names the oracle behind the figures and refuses the market-cap reading', async () => {
    apiGetData.mockResolvedValue(view());
    renderPanel();
    expect(
      await screen.findByText(/Not a market capitalisation/),
    ).toBeInTheDocument();
    expect(screen.getByText(/redstone/)).toBeInTheDocument();
    expect(
      screen.getByText(/Membership is today’s set applied backwards/),
    ).toBeInTheDocument();
  });

  it('accounts for the members left out of the series', async () => {
    apiGetData.mockResolvedValue(
      view({
        excluded: [
          {
            reason: 'not_bound',
            assets: 4,
            detail: 'No curated binding.',
          },
          {
            reason: 'contract_not_bound',
            assets: 1,
            detail: 'Contract-issued.',
          },
        ],
      }),
    );
    renderPanel();
    expect(
      await screen.findByText(/4 with no oracle feed bound to their exact/),
    ).toBeInTheDocument();
  });

  it('renders an explanation, never an empty plot, when no series exists', async () => {
    apiGetData.mockResolvedValue(
      view({
        points: [],
        basis: 'No series is published: the reads did not answer.',
      }),
    );
    renderPanel();
    expect(
      await screen.findByText(/No value series is published/),
    ).toBeInTheDocument();
    expect(screen.getByText(/the reads did not answer/)).toBeInTheDocument();
    expect(screen.queryByRole('img')).not.toBeInTheDocument();
  });

  it('carries the coverage into the chart’s text alternative', async () => {
    apiGetData.mockResolvedValue(view());
    renderPanel();
    const chart = await screen.findByRole('img');
    expect(chart.getAttribute('aria-label')).toMatch(
      /across 6 of 11 assets, a lower bound/,
    );
  });

  it('reports a failed load rather than an empty chart', async () => {
    apiGetData.mockRejectedValue(new Error('upstream refused'));
    renderPanel();
    expect(
      await screen.findByText(/Failed to load the value history/),
    ).toBeInTheDocument();
    expect(screen.getByText(/upstream refused/)).toBeInTheDocument();
  });

  it('offers the window and decomposition controls', async () => {
    apiGetData.mockResolvedValue(view());
    renderPanel();
    expect(
      await screen.findByRole('group', { name: 'Chart window' }),
    ).toBeInTheDocument();
    expect(
      screen.getByRole('group', { name: 'Series decomposition' }),
    ).toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'All' })).toBeInTheDocument();
    expect(
      screen.getByRole('button', { name: 'By asset' }),
    ).toBeInTheDocument();
  });

  it('gives every per-asset line a named legend entry, not colour alone', async () => {
    apiGetData.mockResolvedValue(
      view({
        groups: [
          {
            key: `USDY-${ISSUER}`,
            code: 'USDY',
            issuer: ISSUER,
            feed: 'rwa:USDY',
            source: 'redstone',
            members: 1,
            points: [point({ value_usd: '535000000.00' })],
          },
          {
            key: `USTRY-${ISSUER}`,
            code: 'USTRY',
            issuer: ISSUER,
            feed: 'rwa:USTRY',
            source: 'redstone',
            members: 1,
            points: [point({ value_usd: '1300000.00' })],
          },
        ],
      }),
    );
    renderPanel();
    fireEvent.click(await screen.findByRole('button', { name: 'By asset' }));
    // Identity is carried in text beside the swatch, so a reader who
    // cannot separate two hues can still tell the lines apart.
    expect(await screen.findByText('USDY')).toBeInTheDocument();
    expect(screen.getByText('USTRY')).toBeInTheDocument();
    // And the request actually asked the server to decompose.
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
