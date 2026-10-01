import { describe, it, expect, vi } from 'vitest';
import { render, screen } from '@testing-library/react';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';

vi.mock('@/lib/buildFetch', async () => {
  const actual =
    await vi.importActual<typeof import('@/lib/buildFetch')>(
      '@/lib/buildFetch',
    );
  return { ...actual, buildFetchData: vi.fn(), failBuild: vi.fn() };
});

// Client children of this server page pull their own data; stub them so
// the assertions below are about the build-time market list only.
vi.mock('./SourceHealthPanel', () => ({ SourceHealthPanel: () => <div /> }));
vi.mock('../../dexes/[source]/SourceVolumeHistory', () => ({
  SourceVolumeHistory: () => <div />,
}));
const { topCharts } = vi.hoisted(() => ({ topCharts: [] as string[] }));
vi.mock('../../dexes/[source]/SourceTopChart', () => ({
  SourceTopChart: ({ source }: { source: string }) => {
    topCharts.push(source);
    return <div />;
  },
}));

import { buildFetchData, failBuild } from '@/lib/buildFetch';
import SourceDetailPage, {
  generateMetadata,
  generateStaticParams,
} from './page';

const SOURCE = {
  name: 'soroswap',
  class: 'exchange',
  subclass: 'dex',
  on_chain: true,
};
const CEX = {
  name: 'binance',
  class: 'exchange',
  subclass: 'cex',
  on_chain: false,
};

function mockFetches(markets: unknown) {
  vi.mocked(buildFetchData).mockImplementation(async (path: string) => {
    if (path.startsWith('/v1/markets')) return markets as never;
    if (path.startsWith('/v1/sources')) return [SOURCE, CEX] as never;
    return [] as never; // /v1/diagnostics/cursors
  });
}

async function renderPage(name = 'soroswap') {
  const tree = await SourceDetailPage({
    params: Promise.resolve({ name }),
  });
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  });
  render(<QueryClientProvider client={client}>{tree}</QueryClientProvider>);
}

// Frontend-honesty sweep: the top-markets panel coalesced a null read to
// `[]` and baked "0 pairs" + "No markets observed for this source in the
// trailing 14 days" into the static export — a claim about the venue
// made from a query that never answered.
describe('SourceDetailPage top markets', () => {
  it('says the market list is unavailable when the read returns null', async () => {
    mockFetches(null);
    await renderPage();
    expect(
      screen.getByText(/Market list unavailable for this build/),
    ).toBeInTheDocument();
    expect(
      screen.queryByText(/No markets observed for this source/),
    ).not.toBeInTheDocument();
    expect(screen.getByText(/— pairs/)).toBeInTheDocument();
  });

  it('keeps the genuine empty claim when the API returns no markets', async () => {
    mockFetches([]);
    await renderPage();
    expect(
      screen.getByText(/No markets observed for this source/),
    ).toBeInTheDocument();
    expect(
      screen.queryByText(/Market list unavailable/),
    ).not.toBeInTheDocument();
    expect(screen.getByText(/0 pairs/)).toBeInTheDocument();
  });
});

// /v1/markets?source= answers 400 for an off-chain source: its prices are
// served only blended with other sources, so the page must not select it.
describe('SourceDetailPage per-source markets', () => {
  const marketCalls = () =>
    vi
      .mocked(buildFetchData)
      .mock.calls.map(([path]) => path)
      .filter((path) => path.startsWith('/v1/markets'));

  it('never selects an off-chain source on /v1/markets', async () => {
    mockFetches([]);
    vi.mocked(buildFetchData).mockClear();
    topCharts.length = 0;
    await renderPage('binance');
    expect(marketCalls()).toEqual([]);
    expect(topCharts).toEqual([]);
    expect(
      screen.queryByText('Top markets via this source'),
    ).not.toBeInTheDocument();
  });

  it('keeps the per-source markets for an on-chain source', async () => {
    mockFetches([]);
    vi.mocked(buildFetchData).mockClear();
    topCharts.length = 0;
    await renderPage('soroswap');
    expect(marketCalls()).toEqual([
      '/v1/markets?source=soroswap&order_by=volume_24h_usd_desc&limit=25',
    ]);
    expect(topCharts).toEqual(['soroswap']);
  });
});

// T291: functions/sources/[[path]].js serves /sources/shell/ for every
// source registered after the build, so the build must bake that document
// and it must not be the fail-hard "promised but unlisted" path.
describe('SourceDetailPage runtime shell', () => {
  it('bakes the shell sentinel alongside the listed sources', async () => {
    mockFetches([]);
    expect(await generateStaticParams()).toEqual([
      { name: 'soroswap' },
      { name: 'binance' },
      { name: 'shell' },
    ]);
  });

  it('renders the shell without a build fetch or a build failure', async () => {
    vi.mocked(buildFetchData).mockClear();
    vi.mocked(failBuild).mockClear();
    await SourceDetailPage({ params: Promise.resolve({ name: 'shell' }) });
    expect(buildFetchData).not.toHaveBeenCalled();
    expect(failBuild).not.toHaveBeenCalled();
  });

  it('keeps the shell out of the index and clears the canonical', async () => {
    const meta = await generateMetadata({
      params: Promise.resolve({ name: 'shell' }),
    });
    expect(meta.robots).toMatchObject({ index: false });
    expect(meta).toHaveProperty('alternates');
    expect(meta.alternates?.canonical).toBeUndefined();
  });
});
