import { describe, it, expect, vi } from 'vitest';
import { render } from '@testing-library/react';

const net = vi.hoisted(() => ({ id: 'mainnet' }));
vi.mock('@/lib/networks', async (importOriginal) => {
  const actual = await importOriginal<typeof import('@/lib/networks')>();
  return {
    ...actual,
    get CURRENT_NETWORK() {
      return actual.NETWORKS.find((n) => n.id === net.id)!;
    },
  };
});

import WidgetsPage from './page';

function assetCardSlugs() {
  const { container } = render(<WidgetsPage />);
  return Array.from(container.querySelectorAll('iframe'))
    .map((f) => f.getAttribute('src') ?? '')
    .filter((src) => src.includes('/embed/asset/'))
    .map((src) => src.split('/embed/asset/')[1]);
}

// An example card for an asset the network does not carry renders "not
// found" inside the iframe, so each network shows only its own assets.
describe('widgets asset examples', () => {
  it.each([
    ['mainnet', ['XLM', 'USDC', 'AQUA']],
    ['testnet', ['XLM', 'USDC']],
    ['futurenet', ['XLM']],
  ])('%s shows %j', (id, want) => {
    net.id = id;
    expect(assetCardSlugs()).toEqual(want);
  });
});
