import { describe, expect, it } from 'vitest';

import {
  MAX_CONVERT_ASSETS,
  buildAssetConvertParams,
  buildConvertParams,
  convertAssets,
  convertQuery,
} from './convert-params';

const USDC_ID = 'USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN'; // gitleaks:allow — public Circle issuer

describe('buildConvertParams', () => {
  it('never emits a hub ticker the catalogue does not serve', () => {
    // PLN is a hub but absent here: baking /convert/PLN/* fails the export
    // because the identity read 404s/400s for an unserved ticker.
    const pairs = buildConvertParams(['USD', 'EUR', 'ISK']);
    const seen = new Set(pairs.flatMap((p) => [p.from, p.to]));
    expect(seen.has('PLN')).toBe(false);
    expect(pairs).toContainEqual({ from: 'USD', to: 'ISK' });
    expect(pairs).toContainEqual({ from: 'ISK', to: 'EUR' });
    expect(pairs.some((p) => p.from === p.to)).toBe(false);
  });
});

describe('convertQuery', () => {
  it('reads fiat and XLM forward when the quote is fiat', () => {
    expect(convertQuery('EUR', 'USD')).toEqual({
      asset: 'fiat:EUR',
      quote: 'fiat:USD',
      invert: false,
    });
    expect(convertQuery('XLM', 'USD')).toEqual({
      asset: 'native',
      quote: 'fiat:USD',
      invert: false,
    });
  });

  it('reads fiat to XLM as XLM in that fiat, inverted', () => {
    expect(convertQuery('USD', 'XLM')).toEqual({
      asset: 'native',
      quote: 'fiat:USD',
      invert: true,
    });
  });
});

describe('convertQuery with verified assets', () => {
  const ids = { USDC: USDC_ID };

  it('reads a verified asset by its (code, issuer) id, never its code', () => {
    expect(convertQuery('USDC', 'EUR', ids)).toEqual({
      asset: USDC_ID,
      quote: 'fiat:EUR',
      invert: false,
    });
    expect(convertQuery('XLM', 'USDC', ids)).toEqual({
      asset: 'native',
      quote: USDC_ID,
      invert: false,
    });
  });

  it('reads fiat to a verified asset as the asset in that fiat, inverted', () => {
    expect(convertQuery('EUR', 'USDC', ids)).toEqual({
      asset: USDC_ID,
      quote: 'fiat:EUR',
      invert: true,
    });
  });
});

describe('convertAssets', () => {
  it('keeps only catalogue credit assets with an issuer, deduped by URL key', () => {
    const assets = convertAssets(
      [
        { ticker: 'XLM', asset_id: 'native' },
        {
          ticker: 'USDC',
          name: 'USD Coin',
          slug: 'usdc',
          asset_id: USDC_ID,
          issuer: 'GA5Z',
        },
        { ticker: 'USDT' },
        { ticker: 'USD' },
        { ticker: 'usdc', asset_id: 'usdc-GSCAM', issuer: 'GSCAM' },
        { ticker: 'yXLM', asset_id: 'yXLM-GARD', issuer: 'GARD' },
      ],
      ['USD'],
    );
    expect(assets.map((a) => [a.ticker, a.assetId])).toEqual([
      ['USDC', USDC_ID],
      ['yXLM', 'yXLM-GARD'],
    ]);
  });

  it('caps the list', () => {
    const rows = Array.from({ length: MAX_CONVERT_ASSETS + 5 }, (_, i) => ({
      ticker: `A${i}X`,
      asset_id: `A${i}X-G${i}`,
      issuer: `G${i}`,
    }));
    expect(convertAssets(rows, [])).toHaveLength(MAX_CONVERT_ASSETS);
  });
});

describe('buildAssetConvertParams', () => {
  it('bakes asset → served hub only, under the upper-cased ticker', () => {
    const pairs = buildAssetConvertParams(
      [{ ticker: 'yXLM', assetId: 'yXLM-GARD', name: 'yXLM', slug: 'yxlm' }],
      ['USD', 'ISK'],
    );
    expect(pairs).toEqual([{ from: 'YXLM', to: 'USD' }]);
  });
});
