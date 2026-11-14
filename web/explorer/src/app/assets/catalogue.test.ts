import { describe, expect, it, vi } from 'vitest';

const USDC = 'USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN';
const FAKE = 'USDC-GDC7X2MXTYSAKUUGAIQ7J7RPEIM7GXSAIWFYWWH4GLNFECQVJJLB2EEU';

vi.mock('@/lib/buildFetch', () => ({
  failBuild: vi.fn(),
  buildFetchData: vi.fn(async () => [
    {
      ticker: 'USDC',
      slug: 'usdc',
      name: 'USD Coin',
      class: 'stablecoin',
      asset_id: USDC,
    },
    { ticker: 'USD', slug: 'us-dollar', name: 'US Dollar', class: 'fiat' },
  ]),
}));

import { lookupGlobalAsset, lookupGlobalAssetByAssetID } from './catalogue';

describe('catalogue lookups', () => {
  it('resolves a verified issuance by its asset_id to the catalogue entry', async () => {
    const bySlug = await lookupGlobalAsset('usdc');
    expect(bySlug?.name).toBe('USD Coin');
    expect(await lookupGlobalAssetByAssetID(USDC)).toEqual(bySlug);
  });

  it('does not match a same-code asset from another issuer', async () => {
    expect(await lookupGlobalAssetByAssetID(FAKE)).toBeNull();
    expect(await lookupGlobalAssetByAssetID('usdc')).toBeNull();
  });
});
