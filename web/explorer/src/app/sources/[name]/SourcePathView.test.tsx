import { describe, it, expect, vi, beforeEach } from 'vitest';
import { render, screen, waitFor } from '@testing-library/react';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';

const { seen, recordSource, apiGet } = vi.hoisted(() => {
  const seen: string[] = [];
  const recordSource = ({ source }: { source: string }) => {
    seen.push(source);
    return null;
  };
  const apiGet = vi.fn();
  return { seen, recordSource, apiGet };
});
vi.mock('./SourceHealthPanel', () => ({ SourceHealthPanel: recordSource }));
vi.mock('@/app/dexes/[source]/SourceStatsPanel', () => ({
  SourceStatsPanel: recordSource,
}));
vi.mock('@/api/client', async () => ({
  ...(await vi.importActual<typeof import('@/api/client')>('@/api/client')),
  apiGet,
}));

import { SourcePathView } from './SourcePathView';

function serve(onChain: boolean) {
  apiGet.mockImplementation(async (path: string) =>
    path === '/v1/sources'
      ? { data: [{ name: 'newvenue', class: 'exchange', on_chain: onChain }] }
      : { data: [] },
  );
}

function renderView() {
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  });
  return render(
    <QueryClientProvider client={client}>
      <SourcePathView />
    </QueryClientProvider>,
  );
}

const marketCalls = () =>
  apiGet.mock.calls.filter(([path]) => path === '/v1/markets');

// The runtime fallback for a source registered after the last build (T291).
describe('SourcePathView', () => {
  beforeEach(() => {
    seen.length = 0;
    apiGet.mockReset();
    window.history.pushState({}, '', '/sources/newvenue/');
  });

  it('renders every live panel for an on-chain name read from the URL', async () => {
    serve(true);
    renderView();
    expect(
      screen.getByRole('heading', { level: 1, name: 'newvenue' }),
    ).toBeInTheDocument();
    expect(seen).toEqual(['newvenue', 'newvenue']);
    await waitFor(() => expect(marketCalls()).toHaveLength(1));
    expect(marketCalls()[0][1]).toMatchObject({ source: 'newvenue' });
  });

  it('never selects an off-chain source on /v1/markets', async () => {
    serve(false);
    renderView();
    await waitFor(() =>
      expect(apiGet.mock.calls.some(([path]) => path === '/v1/sources')).toBe(
        true,
      ),
    );
    // Give the registry answer the same time the on-chain case needs to
    // reach /v1/markets, so absence is not just "not yet".
    await new Promise((r) => setTimeout(r, 50));
    expect(marketCalls()).toEqual([]);
  });

  it('does not throw on a malformed percent-escape segment', () => {
    serve(true);
    window.history.pushState({}, '', '/sources/%ZZ');
    expect(() => renderView()).not.toThrow();
  });
});
