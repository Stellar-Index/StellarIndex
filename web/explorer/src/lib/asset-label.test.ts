import { describe, it, expect } from 'vitest';

import { assetSlug } from '@/components/AssetLink';
import { isRawOracleAsset, rawOracleSymbol, shortAssetText } from './asset-label';

// Oracle capture-totality: a `raw:<symbol>` id is an oracle-published
// symbol recorded verbatim because it maps to no canonical asset. It has no
// asset page by definition, so the slug is null (before this branch the
// classic split linked it to /assets/raw%3ANOTACOIN — a static-export 404)
// and the label is the on-wire symbol, never truncated.
describe('raw: oracle asset ids', () => {
  it('assetSlug refuses to link an unmapped symbol', () => {
    expect(assetSlug('raw:NOTACOIN')).toBeNull();
    expect(assetSlug('raw:SolvBTC.BBN_FUNDAMENTAL/USD')).toBeNull();
    // Mapped forms are unaffected.
    expect(assetSlug('crypto:BTC')).toBe('BTC');
    expect(assetSlug('fiat:USD')).toBe('USD');
  });

  it('shortAssetText renders the on-wire symbol verbatim', () => {
    expect(shortAssetText('raw:NOTACOIN')).toBe('NOTACOIN');
    expect(shortAssetText('raw:SolvBTC.BBN_FUNDAMENTAL/USD')).toBe('SolvBTC.BBN_FUNDAMENTAL/USD');
  });

  it('isRawOracleAsset / rawOracleSymbol', () => {
    expect(isRawOracleAsset('raw:X')).toBe(true);
    expect(isRawOracleAsset('crypto:X')).toBe(false);
    expect(isRawOracleAsset(undefined)).toBe(false);
    expect(rawOracleSymbol('raw:NOTACOIN')).toBe('NOTACOIN');
  });
});
