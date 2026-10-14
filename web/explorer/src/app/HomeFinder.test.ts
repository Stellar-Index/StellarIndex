import { describe, expect, it } from 'vitest';

import { destinationFor, detectShape } from './HomeFinder';

const G = 'G' + 'A'.repeat(55);
const C = 'C' + 'B'.repeat(55);
const TX = 'ab'.repeat(32);

describe('HomeFinder routing', () => {
  it('routes each identifier shape to its entity page', () => {
    expect(destinationFor(G)).toBe(`/accounts/${G}/`);
    expect(destinationFor(C)).toBe(`/contracts/${C}/`);
    expect(destinationFor(TX.toUpperCase())).toBe(`/transactions/${TX}/`);
    expect(destinationFor(' 58123456 ')).toBe('/ledgers/58123456/');
  });

  it('sends asset codes and pairs to the assets directory search', () => {
    expect(detectShape('USDC')).toBe('asset');
    expect(detectShape('XLM/USD')).toBe('pair');
    expect(destinationFor('XLM/USD')).toBe('/assets/?q=XLM%2FUSD');
  });

  it('does nothing for an empty query', () => {
    expect(detectShape('   ')).toBeNull();
    expect(destinationFor('')).toBeNull();
  });
});
