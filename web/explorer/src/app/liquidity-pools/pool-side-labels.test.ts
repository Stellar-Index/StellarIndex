import { describe, expect, it } from 'vitest';

import { poolSideLabels } from './PoolDepthDetail';

const G1 = 'GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN';
const G2 = 'GDQOE23CFSUMSVQK4Y5JHPPYK73VYCNHZHA7ENKCV37P6SUEO6XQBKPP';

describe('poolSideLabels', () => {
  it('uses bare codes when the two sides differ', () => {
    expect(poolSideLabels('native', `USDC-${G1}`)).toEqual(['XLM', 'USDC']);
  });

  it('adds the issuer when both sides share a code', () => {
    expect(poolSideLabels(`USDC-${G1}`, `USDC-${G2}`)).toEqual([
      'USDC (GA5Z…KZVN)',
      'USDC (GDQO…BKPP)',
    ]);
  });
});
