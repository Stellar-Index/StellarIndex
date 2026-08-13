import { describe, it, expect, vi, afterEach } from 'vitest';
import { render, screen } from '@testing-library/react';

import ExternalAssetDetailPage from './page';

function mockFetch(impl: () => Promise<Response>) {
  vi.stubGlobal('fetch', vi.fn(impl));
}

function jsonResponse(body: unknown, status = 200) {
  return new Response(JSON.stringify(body), {
    status,
    headers: { 'content-type': 'application/json' },
  });
}

async function renderPage(slug: string) {
  render(await ExternalAssetDetailPage({ params: Promise.resolve({ slug }) }));
}

// Frontend-honesty sweep: every failure mode collapsed into `null` and
// the page baked "We don't track an external asset with the slug X" —
// a flat denial for an asset generateStaticParams had already confirmed
// exists. Only an authoritative 4xx may produce that claim.
describe('ExternalAssetDetailPage', () => {
  afterEach(() => {
    vi.unstubAllGlobals();
  });

  it('renders "not found" only for an authoritative 404', async () => {
    mockFetch(async () => jsonResponse({}, 404));
    await renderPage('nope');
    expect(screen.getByText('External asset not found')).toBeInTheDocument();
    expect(screen.queryByText('Asset detail unavailable')).not.toBeInTheDocument();
  });

  it('renders "unavailable", not a denial, on a 503', async () => {
    mockFetch(async () => jsonResponse({}, 503));
    await renderPage('btc');
    expect(screen.getByText('Asset detail unavailable')).toBeInTheDocument();
    expect(screen.queryByText('External asset not found')).not.toBeInTheDocument();
  });

  it('renders "unavailable", not a denial, on a transport error', async () => {
    mockFetch(async () => {
      throw new Error('ETIMEDOUT');
    });
    await renderPage('btc');
    expect(screen.getByText('Asset detail unavailable')).toBeInTheDocument();
    expect(screen.queryByText('External asset not found')).not.toBeInTheDocument();
  });

  it('renders the asset when the API answers', async () => {
    mockFetch(async () =>
      jsonResponse({
        data: { slug: 'btc', ticker: 'BTC', name: 'Bitcoin', price_usd: '65000' },
      }),
    );
    await renderPage('btc');
    expect(screen.queryByText('External asset not found')).not.toBeInTheDocument();
    expect(screen.queryByText('Asset detail unavailable')).not.toBeInTheDocument();
  });
});
