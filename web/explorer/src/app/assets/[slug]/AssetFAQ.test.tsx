import { describe, it, expect } from 'vitest';

import { assetFaqFor } from './AssetFAQ';

describe('assetFaqFor', () => {
  it('names native XLM as Stellar Lumens, never "native"', () => {
    const faq = assetFaqFor('XLM', 'native');
    expect(faq.map((f) => f.q)).toContain('What is XLM?');
    const text = faq.map((f) => f.a).join(' ');
    expect(text).toMatch(/Stellar Lumens \(XLM\)/);
    expect(text).toMatch(/XLM has no issuer/);
  });

  it('uses contract-token phrasing for a Soroban token', () => {
    const faq = assetFaqFor('TKN', 'contract');
    expect(faq.map((f) => f.a).join(' ')).toMatch(
      /Soroban-native or smart-contract token/,
    );
  });

  it('uses classic-issuer phrasing when hasIssuer is set', () => {
    const faq = assetFaqFor('USDC', 'classic');
    expect(faq.map((f) => f.q)).toContain('Who issues USDC?');
    expect(faq.map((f) => f.a).join(' ')).toMatch(
      /As a classic credit asset, USDC has a designated issuer account/,
    );
  });
});
