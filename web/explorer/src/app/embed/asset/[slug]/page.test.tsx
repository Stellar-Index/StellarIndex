import { describe, it, expect, vi, afterEach } from 'vitest';
import { render, screen } from '@testing-library/react';

vi.mock('@/lib/buildFetch', () => ({ isCIStub: false }));
vi.mock('../../LivePrice', () => ({ LivePrice: () => null }));
vi.mock('../../LiveChangeChip', () => ({ LiveChangeChip: () => null }));

import EmbedAssetPage from './page';

function stubAsset(volume: string) {
  vi.stubGlobal(
    'fetch',
    vi.fn(async (url: string) => {
      const body = /\/v1\/assets\/USDC$/.test(url)
        ? { data: { code: 'USDC', price_usd: '1', volume_24h_usd: volume } }
        : { data: [] };
      return new Response(JSON.stringify(body), { status: 200 });
    }),
  );
}

async function renderEmbed() {
  render(await EmbedAssetPage({ params: Promise.resolve({ slug: 'USDC' }) }));
}

describe('EmbedAssetPage 24h volume', () => {
  afterEach(() => vi.unstubAllGlobals());

  it.each([
    ['999.994999999999998', '$999.99 24h vol'],
    ['1500000000', '$1.5B 24h vol'],
  ])('renders %s exactly as %s', async (volume, shown) => {
    stubAsset(volume);
    await renderEmbed();
    expect(screen.getByText(shown)).toBeInTheDocument();
  });

  it.each(['1e9', '0', '-5'])('omits a volume of %s', async (volume) => {
    stubAsset(volume);
    await renderEmbed();
    expect(screen.queryByText(/24h vol/)).toBeNull();
  });
});
