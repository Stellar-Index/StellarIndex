import { describe, expect, it } from 'vitest';

import { offChainAssetHref } from './assetRoute';

describe('offChainAssetHref', () => {
  it('sends crypto: and fiat: ids to the external asset page', () => {
    expect(offChainAssetHref('crypto:BTC')).toBe('/external/assets/btc/');
    expect(offChainAssetHref('fiat:EUR')).toBe('/external/assets/eur/');
  });
  it('sends raw oracle symbols to the oracle view', () => {
    expect(offChainAssetHref('raw:BTC')).toBe('/oracles/');
  });
  it('leaves Stellar ids alone', () => {
    expect(offChainAssetHref('native')).toBeNull();
    expect(offChainAssetHref('USDC-GA5Z')).toBeNull();
  });
});
