import { describe, it, expect, vi, afterEach } from 'vitest';
import { render, screen } from '@testing-library/react';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';

import { ExternalAssetPathView } from './ExternalAssetPathView';

// The runtime fallback for a reference asset added after the last build
// (T291).
function renderAt(pathname: string, status: number, body: unknown) {
  const fetchSpy = vi.fn(
    async (_input: RequestInfo | URL) =>
      new Response(JSON.stringify(body), {
        status,
        headers: { 'content-type': 'application/json' },
      }),
  );
  vi.stubGlobal('fetch', fetchSpy);
  window.history.pushState({}, '', pathname);
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  });
  render(
    <QueryClientProvider client={client}>
      <ExternalAssetPathView />
    </QueryClientProvider>,
  );
  return fetchSpy;
}

describe('ExternalAssetPathView', () => {
  afterEach(() => {
    vi.unstubAllGlobals();
  });

  it('renders the asset the URL names, fetched live', async () => {
    const fetchSpy = renderAt('/external/assets/new-coin/', 200, {
      data: { slug: 'new-coin', ticker: 'NEW', name: 'New Coin' },
    });
    await screen.findAllByText('New Coin');
    expect(document.querySelector('h1')?.textContent).toContain('New Coin');
    expect(String(fetchSpy.mock.calls[0][0])).toContain(
      '/v1/external/assets/new-coin',
    );
  });

  it('says "not found" only for an authoritative 404', async () => {
    renderAt('/external/assets/nope/', 404, {});
    expect(
      await screen.findByText('External asset not found'),
    ).toBeInTheDocument();
  });

  it('says "unavailable", not a denial, on a 503', async () => {
    renderAt('/external/assets/new-coin/', 503, {});
    expect(
      await screen.findByText('Asset detail unavailable'),
    ).toBeInTheDocument();
    expect(
      screen.queryByText('External asset not found'),
    ).not.toBeInTheDocument();
  });
});
