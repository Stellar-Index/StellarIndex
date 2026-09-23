import { describe, it, expect, vi, afterEach } from 'vitest';
import { render, screen } from '@testing-library/react';

import ExternalAssetDetailPage, {
  generateMetadata,
  generateStaticParams,
} from './page';

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
    expect(
      screen.queryByText('Asset detail unavailable'),
    ).not.toBeInTheDocument();
  });

  it('renders "unavailable", not a denial, on a 503', async () => {
    mockFetch(async () => jsonResponse({}, 503));
    await renderPage('btc');
    expect(screen.getByText('Asset detail unavailable')).toBeInTheDocument();
    expect(
      screen.queryByText('External asset not found'),
    ).not.toBeInTheDocument();
  });

  it('renders "unavailable", not a denial, on a transport error', async () => {
    mockFetch(async () => {
      throw new Error('ETIMEDOUT');
    });
    await renderPage('btc');
    expect(screen.getByText('Asset detail unavailable')).toBeInTheDocument();
    expect(
      screen.queryByText('External asset not found'),
    ).not.toBeInTheDocument();
  });

  it('renders the asset when the API answers', async () => {
    mockFetch(async () =>
      jsonResponse({
        data: {
          slug: 'btc',
          ticker: 'BTC',
          name: 'Bitcoin',
          price_usd: '65000',
        },
      }),
    );
    await renderPage('btc');
    expect(
      screen.queryByText('External asset not found'),
    ).not.toBeInTheDocument();
    expect(
      screen.queryByText('Asset detail unavailable'),
    ).not.toBeInTheDocument();
  });
});

// T291: functions/external/assets/[[path]].js serves /external/assets/shell/
// for every slug added after the build, so the build must bake it on every
// path (listing, API failure, CI stub) and it must never hit the API.
describe('ExternalAssetDetailPage runtime shell', () => {
  afterEach(() => {
    vi.unstubAllGlobals();
  });

  it('bakes the shell sentinel alongside the listed slugs', async () => {
    mockFetch(async () => jsonResponse({ data: [{ slug: 'eth' }] }));
    expect(await generateStaticParams()).toEqual([
      { slug: 'eth' },
      { slug: 'shell' },
    ]);
  });

  it('bakes the shell sentinel when the listing is unreachable', async () => {
    mockFetch(async () => jsonResponse({}, 503));
    expect(await generateStaticParams()).toContainEqual({ slug: 'shell' });
  });

  it('renders the shell and its metadata without fetching', async () => {
    const fetchSpy = vi.fn(async () => jsonResponse({}, 404));
    vi.stubGlobal('fetch', fetchSpy);
    await ExternalAssetDetailPage({
      params: Promise.resolve({ slug: 'shell' }),
    });
    const meta = await generateMetadata({
      params: Promise.resolve({ slug: 'shell' }),
    });
    expect(fetchSpy).not.toHaveBeenCalled();
    expect(meta.robots).toMatchObject({ index: false });
    expect(meta).toHaveProperty('alternates');
    expect(meta.alternates?.canonical).toBeUndefined();
  });
});
