import { describe, it, expect } from 'vitest';

import { assetFaqFor } from './AssetFAQ';

describe('assetFaqFor', () => {
  it('interpolates the asset code and uses contract-token phrasing without an issuer', () => {
    const faq = assetFaqFor('XLM', false);
    expect(faq.map((f) => f.q)).toContain('What is XLM?');
    expect(faq.map((f) => f.a).join(' ')).toMatch(
      /Soroban-native or smart-contract token/,
    );
  });

  it('uses classic-issuer phrasing when hasIssuer is set', () => {
    const faq = assetFaqFor('USDC', true);
    expect(faq.map((f) => f.q)).toContain('USDC issuer details');
    expect(faq.map((f) => f.a).join(' ')).toMatch(
      /As a classic credit asset, USDC has a designated issuer account/,
    );
  });
});
