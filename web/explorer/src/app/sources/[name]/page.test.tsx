import { describe, it, expect, vi } from 'vitest';
import { render, screen } from '@testing-library/react';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';

vi.mock('@/lib/buildFetch', async () => {
  const actual = await vi.importActual<typeof import('@/lib/buildFetch')>('@/lib/buildFetch');
  return { ...actual, buildFetchData: vi.fn(), failBuild: vi.fn() };
});

// Client children of this server page pull their own data; stub them so
// the assertions below are about the build-time market list only.
vi.mock('./SourceHealthPanel', () => ({ SourceHealthPanel: () => <div /> }));
vi.mock('../../dexes/[source]/SourceVolumeHistory', () => ({
  SourceVolumeHistory: () => <div />,
}));
vi.mock('../../dexes/[source]/SourceTopChart', () => ({ SourceTopChart: () => <div /> }));

import { buildFetchData } from '@/lib/buildFetch';
import SourceDetailPage from './page';

const SOURCE = { name: 'soroswap', class: 'exchange', subclass: 'dex' };

function mockFetches(markets: unknown) {
  vi.mocked(buildFetchData).mockImplementation(async (path: string) => {
    if (path.startsWith('/v1/markets')) return markets as never;
    if (path.startsWith('/v1/sources')) return [SOURCE] as never;
    return [] as never; // /v1/diagnostics/cursors
  });
}

async function renderPage() {
  const tree = await SourceDetailPage({ params: Promise.resolve({ name: 'soroswap' }) });
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
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
    expect(screen.getByText(/Market list unavailable for this build/)).toBeInTheDocument();
    expect(screen.queryByText(/No markets observed for this source/)).not.toBeInTheDocument();
    expect(screen.getByText(/— pairs/)).toBeInTheDocument();
  });

  it('keeps the genuine empty claim when the API returns no markets', async () => {
    mockFetches([]);
    await renderPage();
    expect(screen.getByText(/No markets observed for this source/)).toBeInTheDocument();
    expect(screen.queryByText(/Market list unavailable/)).not.toBeInTheDocument();
    expect(screen.getByText(/0 pairs/)).toBeInTheDocument();
  });
});
