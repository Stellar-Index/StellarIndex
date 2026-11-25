import { describe, it, expect, vi, afterEach } from 'vitest';
import { render, screen } from '@testing-library/react';

vi.mock('../../LivePrice', () => ({ LivePrice: () => null }));
vi.mock('../../LiveChangeChip', () => ({ LiveChangeChip: () => null }));

function stubAsset(volume: string, lowerBound = false) {
  vi.stubGlobal(
    'fetch',
    vi.fn(async (url: string) => {
      const body = /\/v1\/assets\/USDC\?/.test(url)
        ? {
            data: {
              code: 'USDC',
              price_usd: '1',
              volume_24h_usd: volume,
              volume_lower_bound: lowerBound,
            },
          }
        : { data: [] };
      return new Response(JSON.stringify(body), { status: 200 });
    }),
  );
}

// A fresh module per test: the build fetch memoises by URL for the whole build.
async function renderEmbed() {
  const { default: EmbedAssetPage } = await import('./page');
  render(await EmbedAssetPage({ params: Promise.resolve({ slug: 'USDC' }) }));
}

describe('EmbedAssetPage 24h volume', () => {
  afterEach(() => {
    vi.unstubAllGlobals();
    vi.resetModules();
  });

  it.each([
    ['999.994999999999998', '$999.99 24h vol'],
    ['1500000000', '$1.5B 24h vol'],
  ])('renders %s exactly as %s', async (volume, shown) => {
    stubAsset(volume);
    await renderEmbed();
    expect(screen.getByText(shown)).toBeInTheDocument();
  });

  it('marks a volume the API flagged as a lower bound', async () => {
    stubAsset('1500000000', true);
    await renderEmbed();
    expect(screen.getByText('≥ $1.5B 24h vol')).toBeInTheDocument();
  });

  it.each(['1e9', '0', '-5'])('omits a volume of %s', async (volume) => {
    stubAsset(volume);
    await renderEmbed();
    expect(screen.queryByText(/24h vol/)).toBeNull();
  });
});

describe('EmbedAssetPage build fetch', () => {
  afterEach(() => {
    vi.unstubAllGlobals();
    vi.resetModules();
  });

  // Rejected by an edge/WAF: the fail-hard layer throws, the widget degrades.
  it('renders without the asset when every API read is refused, through the shared build fetch', async () => {
    const urls: string[] = [];
    vi.stubGlobal(
      'fetch',
      vi.fn(async (url: string) => {
        urls.push(url);
        return new Response('{}', { status: 403 });
      }),
    );
    await expect(renderEmbed()).resolves.toBeUndefined();
    expect(screen.queryByText(/24h vol/)).toBeNull();
    expect(urls.length).toBeGreaterThan(0);
    // The per-build cache-buster marks a request sent by the shared layer.
    expect(urls.every((u) => /[?&]b=/.test(u))).toBe(true);
  });
});
