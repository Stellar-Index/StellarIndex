import { describe, it, expect } from 'vitest';

import { assetSlug } from '@/components/AssetLink';
import {
  isNativeXlmSac,
  isRawOracleAsset,
  rawOracleSymbol,
  shortAssetText,
} from './asset-label';

// Oracle capture-totality: a `raw:<symbol>` id is an oracle-published
// symbol recorded verbatim because it maps to no canonical asset. It has no
// asset page by definition, so the slug is null (linking it would hit
// /assets/raw%3ANOTACOIN — a static-export 404) and the label is the on-wire symbol, never truncated.
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
    expect(shortAssetText('raw:SolvBTC.BBN_FUNDAMENTAL/USD')).toBe(
      'SolvBTC.BBN_FUNDAMENTAL/USD',
    );
  });

  it('isRawOracleAsset / rawOracleSymbol', () => {
    expect(isRawOracleAsset('raw:X')).toBe(true);
    expect(isRawOracleAsset('crypto:X')).toBe(false);
    expect(isRawOracleAsset(undefined)).toBe(false);
    expect(rawOracleSymbol('raw:NOTACOIN')).toBe('NOTACOIN');
  });
});

describe('isNativeXlmSac', () => {
  const PUB = 'CAS3J7GYLGXMF6TDJBBYYSE3HQ6BBSMLNUQ34T6TZMYMW2EVH34XOWMA';
  const TEST = 'CDLZFC3SYJYDZT7K67VZ75HPJVIEUVNIXF47ZG2FB2RMQQVU2HHGCYSC';
  const FUTURE = 'CB64D3G7SM2RTH6JSGG34DDTFTQ5CFDKVDZJZSODMCX4NJ2HV2KN7OHT';

  it('matches only the SAC of the given network', () => {
    expect(isNativeXlmSac(PUB, 'mainnet')).toBe(true);
    expect(isNativeXlmSac(TEST, 'testnet')).toBe(true);
    expect(isNativeXlmSac(FUTURE, 'futurenet')).toBe(true);
    expect(isNativeXlmSac(PUB, 'testnet')).toBe(false);
    expect(isNativeXlmSac(TEST, 'mainnet')).toBe(false);
    expect(isNativeXlmSac(FUTURE, 'testnet')).toBe(false);
  });

  it('defaults to the build network (mainnet) and rejects empty ids', () => {
    expect(isNativeXlmSac(PUB)).toBe(true);
    expect(isNativeXlmSac('')).toBe(false);
    expect(isNativeXlmSac(undefined)).toBe(false);
  });
});
